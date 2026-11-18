package crdt

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveItems lists txt's live items in order: a format marker as key=value
// with a JSON value, a string as JSON.
func liveItems(t *testing.T, txt *YText) []string {
	t.Helper()
	out := []string{}
	for it := txt.start; it != nil; it = it.Right {
		if it.Deleted {
			continue
		}
		var s string
		switch c := it.Content.(type) {
		case *ContentFormat:
			v, err := json.Marshal(c.Val)
			require.NoError(t, err)
			s = c.Key + "=" + string(v)
		case *ContentString:
			v, err := json.Marshal(c.Str)
			require.NoError(t, err)
			s = string(v)
		default:
			t.Fatalf("unexpected content %T", c)
		}
		out = append(out, s)
	}
	return out
}

// TestUnit_YText_FormatCleanup_PanickingTextObserver checks that a text whose
// own observer panics does not start the remote format cleanup and another
// changed formatted text still does, as in Yjs 13.6.32.
func TestUnit_YText_FormatCleanup_PanickingTextObserver(t *testing.T) {
	marked := []string{"bold=true", "bold=null"}
	cleaned := []string{"bold=null"}
	plain := []string{`"abc"`}
	x := []Delta{{Op: DeltaOpInsert, Insert: "x"}}
	boldX := []Delta{{Op: DeltaOpInsert, Insert: "x", Attributes: Attributes{"bold": true}}}
	xabc := []Delta{{Op: DeltaOpInsert, Insert: "xabc"}}
	tests := []struct {
		name                                 string
		formatted, deleted, observed, panics []string
		deepPanics                           []string
		wantPanic                            []any // the panic is one of these
		wantOrigins                          []any
		wantT1, wantT2                       []string
		wantDeltaT1, wantDeltaT2             []Delta
	}{
		{
			name:      "own observer panics",
			formatted: []string{"t1"}, deleted: []string{"t1"}, observed: []string{"t1"}, panics: []string{"t1"},
			wantPanic: []any{"t1"}, wantOrigins: []any{"remote"},
			wantT1: marked, wantT2: plain, wantDeltaT1: boldX, wantDeltaT2: xabc,
		},
		{
			name:      "own observer returns",
			formatted: []string{"t1"}, deleted: []string{"t1"}, observed: []string{"t1"},
			wantPanic: []any{nil}, wantOrigins: []any{"remote", nil},
			wantT1: cleaned, wantT2: plain, wantDeltaT1: x, wantDeltaT2: xabc,
		},
		{
			name:      "other text's observer returns",
			formatted: []string{"t1", "t2"}, deleted: []string{"t1", "t2"}, observed: []string{"t1", "t2"}, panics: []string{"t1"},
			wantPanic: []any{"t1"}, wantOrigins: []any{"remote", nil},
			wantT1: cleaned, wantT2: cleaned, wantDeltaT1: x, wantDeltaT2: x,
		},
		{
			name:      "other text unobserved",
			formatted: []string{"t1", "t2"}, deleted: []string{"t1", "t2"}, observed: []string{"t1"}, panics: []string{"t1"},
			wantPanic: []any{"t1"}, wantOrigins: []any{"remote", nil},
			wantT1: cleaned, wantT2: cleaned, wantDeltaT1: x, wantDeltaT2: x,
		},
		{
			name:      "other text plain",
			formatted: []string{"t1"}, deleted: []string{"t1", "t2"}, observed: []string{"t1", "t2"}, panics: []string{"t1"},
			wantPanic: []any{"t1"}, wantOrigins: []any{"remote"},
			wantT1: marked, wantT2: []string{}, wantDeltaT1: boldX, wantDeltaT2: x,
		},
		{
			name:      "both observers panic",
			formatted: []string{"t1", "t2"}, deleted: []string{"t1", "t2"}, observed: []string{"t1", "t2"}, panics: []string{"t1", "t2"},
			wantPanic: []any{"t1", "t2"}, wantOrigins: []any{"remote"},
			wantT1: marked, wantT2: marked, wantDeltaT1: boldX, wantDeltaT2: boldX,
		},
		{
			name:      "deep observer panics",
			formatted: []string{"t1"}, deleted: []string{"t1"}, deepPanics: []string{"t1"},
			wantPanic: []any{"deep t1"}, wantOrigins: []any{"remote", nil},
			wantT1: cleaned, wantT2: plain, wantDeltaT1: x, wantDeltaT2: xabc,
		},
		{
			name:      "deep observer panics after a type observer",
			formatted: []string{"t1"}, deleted: []string{"t1"}, observed: []string{"t1"}, deepPanics: []string{"t1"},
			wantPanic: []any{"deep t1"}, wantOrigins: []any{"remote", nil},
			wantT1: cleaned, wantT2: plain, wantDeltaT1: x, wantDeltaT2: xabc,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := newTestDoc(1), newTestDoc(2)
			names := []string{"t1", "t2"}
			a.Transact(func(txn *Transaction) {
				for _, n := range names {
					txn.GetText(n).Insert(txn, 0, "abc", nil)
				}
			})
			syncText(t, a, b, nil)
			b.Transact(func(txn *Transaction) {
				for _, n := range tt.formatted {
					txn.GetText(n).Format(txn, 2, 1, Attributes{"bold": true})
				}
			})
			a.Transact(func(txn *Transaction) {
				for _, n := range tt.deleted {
					txn.GetText(n).Delete(txn, 0, 3)
				}
			})
			var origins []any
			b.OnUpdate(func(_ []byte, origin any) { origins = append(origins, origin) })
			armed := true
			for _, n := range tt.observed {
				b.GetText(n).Observe(func(e YTextEvent) {
					if armed && !e.Txn.Local && slices.Contains(tt.panics, n) {
						panic(n)
					}
				})
			}
			for _, n := range tt.deepPanics {
				b.GetText(n).ObserveDeep(func(txn *Transaction) {
					if armed && !txn.Local {
						panic("deep " + n)
					}
				})
			}

			got := recoverPanic(func() { syncText(t, a, b, "remote") })

			assert.Contains(t, tt.wantPanic, got, "panic")
			assert.Equal(t, tt.wantOrigins, origins, "update origins")
			assert.Equal(t, tt.wantT1, liveItems(t, b.GetText("t1")), "t1 items")
			assert.Equal(t, tt.wantT2, liveItems(t, b.GetText("t2")), "t2 items")
			armed = false
			a.Transact(func(txn *Transaction) {
				for _, n := range names {
					txn.GetText(n).Insert(txn, 0, "x", nil)
				}
			})
			syncText(t, a, b, nil)
			assert.Equal(t, tt.wantDeltaT1, b.GetText("t1").ToDelta(), "t1 delta")
			assert.Equal(t, tt.wantDeltaT2, b.GetText("t2").ToDelta(), "t2 delta")
		})
	}
}

// TestUnit_YText_FormatCleanup_OtherTextObserverPanics checks that, as in
// Yjs, a text observer that gives its text the first format marker starts the
// remote change's cleanup even when another text's observer panics.
func TestUnit_YText_FormatCleanup_OtherTextObserverPanics(t *testing.T) {
	for _, t1Panics := range []bool{false, true} {
		t.Run(fmt.Sprintf("t1Panics=%t", t1Panics), func(t *testing.T) {
			a, b := newTestDoc(1), newTestDoc(2)
			a.Transact(func(txn *Transaction) {
				txn.GetText("t1").Insert(txn, 0, "abc", nil)
				txn.GetText("t2").Insert(txn, 0, "abc", nil)
			})
			syncText(t, a, b, nil)
			a.Transact(func(txn *Transaction) {
				txn.GetText("t1").Insert(txn, 3, "x", nil)
				txn.GetText("t2").Insert(txn, 3, "x", nil)
			})
			var origins []any
			b.OnAfterTransaction(func(txn *Transaction) { origins = append(origins, txn.Origin) })
			b.GetText("t1").Observe(func(e YTextEvent) {
				if !e.Txn.Local && t1Panics {
					panic("t1")
				}
			})
			t2 := b.GetText("t2")
			reacted := false
			t2.Observe(func(e YTextEvent) {
				if e.Txn.Local || reacted {
					return
				}
				reacted = true
				b.Transact(func(txn *Transaction) { t2.Format(txn, 0, 1, Attributes{"bold": true}) }, "react")
			})

			got := recoverPanic(func() { syncText(t, a, b, "remote") })

			if t1Panics {
				assert.Equal(t, "t1", got)
			} else {
				assert.Nil(t, got)
			}
			// ygo runs the observer's transaction at once, Yjs queues it; nil is the cleanup.
			assert.Equal(t, []any{"react", "remote", nil}, origins, "after-transaction origins")
		})
	}
}

// TestUnit_YText_FormatCleanup_DeletedText checks, against Yjs, that a text
// deleted when the remote change commits does not start the format cleanup,
// while one that its own observer deletes still does, as Yjs checks a type's
// deletion before it calls the type's observers.
func TestUnit_YText_FormatCleanup_DeletedText(t *testing.T) {
	tests := []struct {
		name              string
		remoteDeletesText bool
		wantOrigins       []any
	}{
		{"own observer deletes the text", false, []any{"react", "remote", nil}},
		{"remote change deletes the text", true, []any{"remote"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := newTestDoc(1), newTestDoc(2)
			am := a.GetMap("m")
			a.Transact(func(txn *Transaction) { am.Set(txn, "t", NewTextPrelim()) })
			v, _ := am.Get("t")
			at := v.(*YText)
			a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
			syncText(t, a, b, nil)
			bm := b.GetMap("m")
			v, _ = bm.Get("t")
			bt := v.(*YText)
			b.Transact(func(txn *Transaction) { bt.Format(txn, 2, 1, Attributes{"bold": true}) })
			a.Transact(func(txn *Transaction) {
				at.Delete(txn, 0, 3)
				if tt.remoteDeletesText {
					am.Delete(txn, "t")
				}
			})
			var origins []any
			b.OnAfterTransaction(func(txn *Transaction) { origins = append(origins, txn.Origin) })
			if tt.remoteDeletesText {
				bm.Observe(func(YMapEvent) {})
			} else {
				reacted := false
				bt.Observe(func(e YTextEvent) {
					if e.Txn.Local || reacted {
						return
					}
					reacted = true
					b.Transact(func(txn *Transaction) { bm.Delete(txn, "t") }, "react")
				})
			}

			syncText(t, a, b, "remote")

			assert.Equal(t, tt.wantOrigins, origins, "after-transaction origins; nil is the cleanup")
		})
	}
}
