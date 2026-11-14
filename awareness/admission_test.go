package awareness

import (
	"testing"
	"time"

	"github.com/reearth/ygo/encoding"
)

// applyForeign applies update as presence relayed from another node.
func applyForeign(a *Awareness, update []byte, origin any) error {
	_, err := a.ApplyUpdateWith(update, origin, ApplyOptions{Foreign: true})
	return err
}

// rawEntry is one entry of a frame built by rawFrame.
type rawEntry struct {
	id, clock uint64
	state     string
}

// rawFrame encodes an awareness update with exact clocks and states.
func rawFrame(entries ...rawEntry) []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(len(entries)))
		for _, e := range entries {
			enc.WriteVarUint(e.id)
			enc.WriteVarUint(e.clock)
			enc.WriteVarString(e.state)
		}
	})
}

func mustApply(t *testing.T, a *Awareness, frame []byte) {
	t.Helper()
	if err := a.ApplyUpdate(frame, nil); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
}

func isLiveIn(a *Awareness, id uint64) bool {
	_, ok := a.GetStates()[id]
	return ok
}

// At the client cap, a new live client takes the oldest tombstone's slot.
func TestClientCap_LiveClientReclaimsOldestTombstone(t *testing.T) {
	a := New(0)
	a.SetMaxClients(3)
	mustApply(t, a, rawFrame(rawEntry{1, 1, `{}`}, rawEntry{2, 1, `{}`}, rawEntry{3, 1, `{}`}))
	mustApply(t, a, rawFrame(rawEntry{2, 2, "null"}))
	mustApply(t, a, rawFrame(rawEntry{3, 2, "null"}))

	mustApply(t, a, rawFrame(rawEntry{4, 1, `{}`}))
	if !isLiveIn(a, 4) {
		t.Fatal("a live client was refused while the cap held only tombstones to spare")
	}
	if _, ok := a.Meta(2); ok {
		t.Fatal("the oldest tombstone (2) survived; the newer one should have been kept")
	}
	if _, ok := a.Meta(3); !ok {
		t.Fatal("the newer tombstone (3) was reclaimed before the oldest")
	}
	if got := len(a.states); got != 3 {
		t.Fatalf("tracked entries = %d, want the cap of 3", got)
	}
}

// At the cap, an unseen client's null and a state that fails to decode take no
// tombstone's slot.
func TestClientCap_OnlyLiveStateReclaims(t *testing.T) {
	a := New(0)
	a.SetMaxClients(2)
	mustApply(t, a, rawFrame(rawEntry{1, 1, `{}`}, rawEntry{2, 1, `{}`}))
	mustApply(t, a, rawFrame(rawEntry{2, 2, "null"}))

	res, err := a.ApplyUpdateWith(rawFrame(rawEntry{5, 1, "null"}, rawEntry{6, 1, `not json`}), nil, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused {
		t.Fatal("refusals at the cap were not reported")
	}
	if _, ok := a.Meta(2); !ok {
		t.Fatal("a tombstone was reclaimed for an entry that did not end up live")
	}
	for _, id := range []uint64{5, 6} {
		if _, ok := a.Meta(id); ok {
			t.Fatalf("client %d was tracked past the cap", id)
		}
	}
}

// With no tombstone to reclaim, the cap still refuses.
func TestClientCap_AllLiveStillRefuses(t *testing.T) {
	a := New(0)
	a.SetMaxClients(2)
	mustApply(t, a, rawFrame(rawEntry{1, 1, `{}`}, rawEntry{2, 1, `{}`}))
	res, err := a.ApplyUpdateWith(rawFrame(rawEntry{3, 1, `{}`}), nil, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused || isLiveIn(a, 3) {
		t.Fatal("a client was admitted past a cap of live clients")
	}
}

// Relayed churn cannot fill the room with tombstones and shut local clients out
// of the slots the foreign sub-cap reserves for them.
func TestClientCap_RelayedChurnKeepsLocalReservation(t *testing.T) {
	a := New(0)
	a.SetMaxClients(8)
	a.SetMaxForeignClients(4)
	for id := uint64(1); id <= 20; id++ {
		applyForeignEntry(t, a, id, "hi")
		applyForeignEntry(t, a, id, "null")
	}
	mustApply(t, a, rawFrame(rawEntry{1000, 1, `{}`}))
	if !isLiveIn(a, 1000) {
		t.Fatal("relayed churn locked a local client out")
	}
}

// Expiry tombstones are reclaimable too.
func TestClientCap_ExpiredClientsAreReclaimable(t *testing.T) {
	a := New(0)
	a.SetMaxClients(2)
	mustApply(t, a, rawFrame(rawEntry{1, 1, `{}`}, rawEntry{2, 1, `{}`}))
	a.RemoveExpired(0)
	mustApply(t, a, rawFrame(rawEntry{3, 1, `{}`}, rawEntry{4, 1, `{}`}))
	if !isLiveIn(a, 3) || !isLiveIn(a, 4) {
		t.Fatal("expired clients' tombstones were not reclaimed")
	}
}

// Churn that revives and removes the same clients keeps the tombstone queue
// within a constant factor of the tombstones it tracks.
func TestClientCap_TombstoneQueueStaysBounded(t *testing.T) {
	a := New(0)
	for clock := uint64(1); clock <= 2000; clock += 2 {
		mustApply(t, a, rawFrame(rawEntry{1, clock, `{}`}, rawEntry{2, clock, `{}`}))
		mustApply(t, a, rawFrame(rawEntry{1, clock + 1, "null"}, rawEntry{2, clock + 1, "null"}))
	}
	if n := len(a.tombQ); n > 2*len(a.removedAt)+16 {
		t.Fatalf("tombQ holds %d entries for %d tombstones", n, len(a.removedAt))
	}
}

// Admit sees each entry's real outcome under the lock: a stale removal never
// reaches it, and an assertion for a client whose tombstone was purged arrives
// as an introduction, whatever its clock.
func TestApplyUpdateWith_AdmitSeesRealOutcome(t *testing.T) {
	a := New(0)
	mustApply(t, a, rawFrame(rawEntry{1, 5, `{}`}, rawEntry{2, 10, "null"}))
	a.removedAt[2] = a.removedAt[2].Add(-time.Hour)
	if a.PurgeTombstones(time.Minute) != 1 {
		t.Fatal("setup: tombstone not purged")
	}

	var seen []AdmitEntry
	admit := func(e AdmitEntry, _ func(uint64) bool) bool {
		seen = append(seen, e)
		return true
	}
	_, err := a.ApplyUpdateWith(rawFrame(
		rawEntry{1, 4, "null"}, // stale: discarded before Admit
		rawEntry{2, 1, `{}`},   // purged tombstone: an introduction
		rawEntry{1, 6, "null"}, // honoured removal
	), nil, ApplyOptions{Admit: admit})
	if err != nil {
		t.Fatal(err)
	}
	want := []AdmitEntry{
		{ClientID: 2, WasLive: false, Live: true},
		{ClientID: 1, WasLive: true, Live: false},
	}
	if len(seen) != len(want) {
		t.Fatalf("Admit saw %+v, want %+v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("Admit saw %+v, want %+v", seen, want)
		}
	}
}

// An entry Admit refuses is skipped alone; the result lists what was applied.
func TestApplyUpdateWith_RefusalSkipsOnlyThatEntry(t *testing.T) {
	a := New(0)
	admit := func(e AdmitEntry, _ func(uint64) bool) bool { return e.ClientID != 2 }
	res, err := a.ApplyUpdateWith(rawFrame(rawEntry{1, 1, `{}`}, rawEntry{2, 1, `{}`}, rawEntry{3, 1, `{}`}), nil,
		ApplyOptions{Admit: admit})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused {
		t.Fatal("refusal not reported")
	}
	if len(res.Applied) != 2 || res.Applied[0] != 1 || res.Applied[1] != 3 {
		t.Fatalf("Applied = %v, want [1 3]", res.Applied)
	}
	if !isLiveIn(a, 1) || isLiveIn(a, 2) || !isLiveIn(a, 3) {
		t.Fatal("refusal was not confined to its entry")
	}
}

// isLive reflects the entries already applied in the same update.
func TestApplyUpdateWith_IsLiveSeesEarlierEntries(t *testing.T) {
	a := New(0)
	mustApply(t, a, rawFrame(rawEntry{1, 1, `{}`}))
	var liveAtSecond bool
	admit := func(e AdmitEntry, isLive func(uint64) bool) bool {
		if e.ClientID == 2 {
			liveAtSecond = isLive(1)
		}
		return true
	}
	if _, err := a.ApplyUpdateWith(rawFrame(rawEntry{1, 2, "null"}, rawEntry{2, 1, `{}`}), nil,
		ApplyOptions{Admit: admit}); err != nil {
		t.Fatal(err)
	}
	if liveAtSecond {
		t.Fatal("isLive did not see the removal earlier in the update")
	}
}

// Altered is set whenever the room holds something other than what an entry
// said; an update applied as sent leaves both flags clear.
func TestApplyUpdateWith_AlteredReportsEntriesNotTakenAsSent(t *testing.T) {
	cases := []struct {
		name  string
		entry rawEntry
		want  bool
	}{
		{"applied as sent", rawEntry{2, 1, `{}`}, false},
		{"stale", rawEntry{1, 4, `{}`}, true},
		{"equal clock", rawEntry{1, 5, `{"x":1}`}, true},
		{"not an object", rawEntry{2, 1, `[1]`}, true},
		{"empty state for a live client", rawEntry{1, 6, ""}, true},
		{"null for a live client", rawEntry{1, 6, "null"}, false},
		{"null for an unseen client", rawEntry{2, 1, "null"}, true},
		{"null for the local client", rawEntry{0, 9, "null"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := New(0)
			mustApply(t, a, rawFrame(rawEntry{1, 5, `{}`}))
			res, err := a.ApplyUpdateWith(rawFrame(c.entry), nil, ApplyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Altered != c.want || res.Refused {
				t.Fatalf("Altered=%v Refused=%v, want Altered=%v", res.Altered, res.Refused, c.want)
			}
		})
	}
}
