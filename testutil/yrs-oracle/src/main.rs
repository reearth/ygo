// yrs-oracle — replays fuzz Scenarios against yrs (Rust port of Yjs), one
// NDJSON line in/out. Same wire contract as testutil/fuzz_oracle.js
// (consumed by testutil/fuzz/oracle_node.go's NodeResult) but restricted to
// ARRAY-ONLY scenarios (root "a") per Task C2 — non-array ops are ignored
// defensively should any slip through the Go-side scenario filter.
//
// MUST mirror testutil/fuzz/interpret.go's transport discipline exactly
// (see fuzz_oracle.js's header comment for the full rationale):
//
//   * Diff*/Merge* updates are staged in per-peer, per-wire-version inboxes
//     (inbox_v1/inbox_v2) — V1 and V2 are distinct, non-interchangeable
//     encodings, so an update is only ever merged/applied through its own
//     decoder, never guessed.
//   * Apply* syncs land immediately using an sv-diff of the source against
//     the destination's state vector.
//   * Merge* pushes the source's FULL state into the version bucket, merges
//     the WHOLE bucket, applies it, and clears the bucket (draining any
//     Diff* entries already staged there too).
//   * After the step list: drainInboxes applies every staged update one at a
//     time through its own decoder, then finalSync forces a full-mesh V1
//     sync (encode each peer's full state, merge them all, apply the merge
//     to every peer).
use std::io::{self, BufRead, Write};

use serde::Deserialize;
use serde_json::Value as Json;
use yrs::types::ToJson;
use yrs::updates::decoder::Decode;
use yrs::{Any, Array, ArrayRef, Doc, In, Options, ReadTxn, StateVector, Transact, Update};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Scenario {
    num_peers: usize,
    #[serde(default)]
    steps: Vec<Step>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Step {
    kind: String,
    #[serde(default)]
    peer: usize,
    #[serde(default)]
    root: String,
    #[serde(default)]
    type_kind: String,
    #[serde(default)]
    op: String,
    #[serde(default)]
    pos_hint: i64,
    #[serde(default)]
    len_hint: i64,
    #[serde(default)]
    to_hint: i64,
    #[serde(default)]
    json_val: String,
    #[serde(default)]
    from: usize,
    #[serde(default)]
    to: usize,
    #[serde(default)]
    method: String,
}

// clamp mirrors clampIndex(pos, length, forInsert) in interpret.go /
// fuzz_oracle.js EXACTLY: negative -> abs, then modulo (length or length+1
// for inserts), with a `m == 0` guard returning 0.
fn clamp(pos: i64, len: usize, ins: bool) -> u32 {
    let mut p = pos;
    if p < 0 {
        p = -p;
    }
    let m: usize = if ins { len + 1 } else { len };
    if m == 0 {
        0
    } else {
        (p as usize % m) as u32
    }
}

// scalar mirrors ygo's decodeScalar: an empty jsonVal (Step never set one)
// decodes to null; otherwise unmarshal the single JSON scalar. jsonVal is
// always a JSON-encoded scalar (string/number/bool/null) — the array-only
// generator never emits composite values here.
fn scalar(js: &str) -> Any {
    if js.is_empty() {
        return Any::Null;
    }
    match serde_json::from_str::<Json>(js) {
        Ok(Json::Bool(b)) => Any::Bool(b),
        Ok(Json::Number(n)) => Any::Number(n.as_f64().unwrap_or(0.0)),
        Ok(Json::String(s)) => Any::String(s.into()),
        _ => Any::Null,
    }
}

// any_to_json renders a yrs Any (from ArrayRef::get / ToJson) into a
// serde_json::Value, keeping numbers as JSON numbers so they compare equal
// to ygo's peerJSON after the Go side's normalize().
fn any_to_json(v: &Any) -> Json {
    match v {
        Any::Null | Any::Undefined => Json::Null,
        Any::Bool(b) => Json::Bool(*b),
        Any::Number(n) => serde_json::json!(n),
        Any::BigInt(i) => serde_json::json!(i),
        Any::String(s) => Json::String(s.to_string()),
        Any::Buffer(b) => Json::Array(b.iter().map(|x| serde_json::json!(x)).collect()),
        Any::Array(a) => Json::Array(a.iter().map(any_to_json).collect()),
        Any::Map(m) => {
            let mut obj = serde_json::Map::new();
            for (k, v) in m.iter() {
                obj.insert(k.clone(), any_to_json(v));
            }
            Json::Object(obj)
        }
    }
}

// apply_local applies one op Step to a peer's array root. Array-only:
// scenarios never carry a typeKind other than "array", but a non-array
// typeKind is ignored defensively rather than panicking, matching the task
// brief's "shouldn't occur" guard.
fn apply_local(doc: &Doc, st: &Step) {
    if st.type_kind != "array" {
        return;
    }
    let a: ArrayRef = doc.get_or_insert_array(st.root.as_str());
    let mut txn = doc.transact_mut();
    let len = a.len(&txn) as usize;
    match st.op.as_str() {
        "insert" => {
            let idx = clamp(st.pos_hint, len, true);
            a.insert(&mut txn, idx, In::Any(scalar(&st.json_val)));
        }
        "push" => {
            a.push_back(&mut txn, In::Any(scalar(&st.json_val)));
        }
        "delete" => {
            if len > 0 {
                let idx = clamp(st.pos_hint, len, false) as usize;
                let want = if st.len_hint <= 0 {
                    1usize
                } else {
                    st.len_hint as usize
                };
                let n = want.min(len - idx);
                if n > 0 {
                    a.remove_range(&mut txn, idx as u32, n as u32);
                }
            }
        }
        "move" => {
            if len >= 2 {
                let from = clamp(st.pos_hint, len, false);
                let to_ygo = clamp(st.to_hint, len, false);
                // ygo -> yrs index mapping (verified against the yrs spike
                // and ygo's internal physPos): yrs_to = from < to ? to+1 : to.
                let to_yrs = if from < to_ygo { to_ygo + 1 } else { to_ygo };
                a.move_to(&mut txn, from, to_yrs);
            }
        }
        _ => {}
    }
}

struct Peer {
    doc: Doc,
    inbox_v1: Vec<Vec<u8>>,
    inbox_v2: Vec<Vec<u8>>,
}

fn new_peers(n: usize) -> Vec<Peer> {
    (0..n)
        .map(|i| {
            let mut opts = Options::default();
            opts.client_id = (i + 1) as u64;
            Peer {
                doc: Doc::with_options(opts),
                inbox_v1: Vec::new(),
                inbox_v2: Vec::new(),
            }
        })
        .collect()
}

fn apply_v1_update(doc: &Doc, bytes: &[u8]) {
    let update = Update::decode_v1(bytes).expect("decode v1 update");
    let mut txn = doc.transact_mut();
    txn.apply_update(update).expect("apply v1 update");
}

fn apply_v2_update(doc: &Doc, bytes: &[u8]) {
    let update = Update::decode_v2(bytes).expect("decode v2 update");
    let mut txn = doc.transact_mut();
    txn.apply_update(update).expect("apply v2 update");
}

// apply_sync drives one sync step across the from->to peer pair, mirroring
// applySync in interpret.go / the switch in fuzz_oracle.js's runScenario:
//   * applyv1/applyv2 (svApply): sv-diff of `from` against `to`'s state
//     vector, applied immediately through the matching decoder.
//   * diffv1/diffv2: the same sv-diff, staged in `to`'s version-tagged inbox
//     instead of applied immediately.
//   * mergev1/mergev2: push `from`'s FULL state into `to`'s version bucket,
//     merge the whole bucket, apply the merge, then clear the bucket (also
//     draining any diff* entries already staged there).
fn apply_sync(peers: &mut [Peer], fi: usize, ti: usize, method: &str) {
    match method {
        "applyv1" => {
            let sv = peers[ti].doc.transact().state_vector();
            let diff = peers[fi].doc.transact().encode_diff_v1(&sv);
            apply_v1_update(&peers[ti].doc, &diff);
        }
        "applyv2" => {
            let sv = peers[ti].doc.transact().state_vector();
            let diff = peers[fi].doc.transact().encode_diff_v2(&sv);
            apply_v2_update(&peers[ti].doc, &diff);
        }
        "diffv1" => {
            let sv = peers[ti].doc.transact().state_vector();
            let diff = peers[fi].doc.transact().encode_diff_v1(&sv);
            peers[ti].inbox_v1.push(diff);
        }
        "diffv2" => {
            let sv = peers[ti].doc.transact().state_vector();
            let diff = peers[fi].doc.transact().encode_diff_v2(&sv);
            peers[ti].inbox_v2.push(diff);
        }
        "mergev1" => {
            let full = peers[fi]
                .doc
                .transact()
                .encode_state_as_update_v1(&StateVector::default());
            peers[ti].inbox_v1.push(full);
            let merged = yrs::merge_updates_v1(peers[ti].inbox_v1.iter()).expect("merge v1");
            peers[ti].inbox_v1.clear();
            apply_v1_update(&peers[ti].doc, &merged);
        }
        "mergev2" => {
            let full = peers[fi]
                .doc
                .transact()
                .encode_state_as_update_v2(&StateVector::default());
            peers[ti].inbox_v2.push(full);
            let merged = yrs::merge_updates_v2(peers[ti].inbox_v2.iter()).expect("merge v2");
            peers[ti].inbox_v2.clear();
            apply_v2_update(&peers[ti].doc, &merged);
        }
        _ => {}
    }
}

// drain_inboxes applies every staged update left in each peer's inboxes
// after the step list has been replayed, one at a time through its own
// decoder, then clears them.
fn drain_inboxes(peers: &mut [Peer]) {
    for p in peers.iter_mut() {
        let v1 = std::mem::take(&mut p.inbox_v1);
        for u in v1 {
            apply_v1_update(&p.doc, &u);
        }
        let v2 = std::mem::take(&mut p.inbox_v2);
        for u in v2 {
            apply_v2_update(&p.doc, &u);
        }
    }
}

// final_sync forces full transitive connectivity across every peer: each
// peer's complete since-genesis state is merged into one update and applied
// to every peer, mirroring finalSync in interpret.go / fuzz_oracle.js.
fn final_sync(peers: &mut [Peer]) {
    let fulls: Vec<Vec<u8>> = peers
        .iter()
        .map(|p| {
            p.doc
                .transact()
                .encode_state_as_update_v1(&StateVector::default())
        })
        .collect();
    let merged = yrs::merge_updates_v1(fulls.iter()).expect("merge v1 (final sync)");
    for p in peers.iter() {
        apply_v1_update(&p.doc, &merged);
    }
}

fn render(doc: &Doc) -> Json {
    let a: ArrayRef = doc.get_or_insert_array("a");
    let txn = doc.transact();
    let items = any_to_json(&a.to_json(&txn));
    serde_json::json!({ "a": items })
}

fn run_scenario(s: &Scenario) -> Json {
    let n = s.num_peers.max(1);
    let mut peers = new_peers(n);

    for st in &s.steps {
        match st.kind.as_str() {
            "op" => {
                let idx = st.peer % n;
                apply_local(&peers[idx].doc, st);
            }
            "gc" => {
                // ygo forces a commit pass; yrs commits automatically at the
                // end of each transact_mut. No observable logical effect.
            }
            "sync" => {
                let fi = st.from % n;
                let ti = st.to % n;
                apply_sync(&mut peers, fi, ti, st.method.as_str());
            }
            _ => {}
        }
    }

    drain_inboxes(&mut peers);
    final_sync(&mut peers);

    let mut peer_json = Vec::with_capacity(n);
    let mut peer_update_b64 = Vec::with_capacity(n);
    for p in peers.iter() {
        peer_json.push(render(&p.doc));
        let full = p
            .doc
            .transact()
            .encode_state_as_update_v1(&StateVector::default());
        peer_update_b64.push(base64_encode(&full));
    }

    serde_json::json!({ "peerJSON": peer_json, "peerUpdateB64": peer_update_b64 })
}

// base64_encode — minimal standard-alphabet base64 encoder (with padding),
// used only for peerUpdateB64's protocol-symmetry field. No external crate
// needed for this one-shot encode.
fn base64_encode(data: &[u8]) -> String {
    const ALPHABET: &[u8; 64] =
        b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity((data.len() + 2) / 3 * 4);
    let mut chunks = data.chunks_exact(3);
    for c in &mut chunks {
        let n = ((c[0] as u32) << 16) | ((c[1] as u32) << 8) | (c[2] as u32);
        out.push(ALPHABET[((n >> 18) & 0x3f) as usize] as char);
        out.push(ALPHABET[((n >> 12) & 0x3f) as usize] as char);
        out.push(ALPHABET[((n >> 6) & 0x3f) as usize] as char);
        out.push(ALPHABET[(n & 0x3f) as usize] as char);
    }
    let rem = chunks.remainder();
    match rem.len() {
        1 => {
            let n = (rem[0] as u32) << 16;
            out.push(ALPHABET[((n >> 18) & 0x3f) as usize] as char);
            out.push(ALPHABET[((n >> 12) & 0x3f) as usize] as char);
            out.push('=');
            out.push('=');
        }
        2 => {
            let n = ((rem[0] as u32) << 16) | ((rem[1] as u32) << 8);
            out.push(ALPHABET[((n >> 18) & 0x3f) as usize] as char);
            out.push(ALPHABET[((n >> 12) & 0x3f) as usize] as char);
            out.push(ALPHABET[((n >> 6) & 0x3f) as usize] as char);
            out.push('=');
        }
        _ => {}
    }
    out
}

fn main() {
    let stdin = io::stdin();
    let stdout = io::stdout();
    let mut out = stdout.lock();
    for line in stdin.lock().lines() {
        let line = line.expect("read stdin line");
        if line.trim().is_empty() {
            continue;
        }
        let scenario: Scenario = serde_json::from_str(&line).expect("parse scenario JSON");
        let result = run_scenario(&scenario);
        writeln!(out, "{}", result).expect("write stdout line");
    }
}
