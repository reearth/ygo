package crdt

// ClientID uniquely identifies a collaborating peer.
type ClientID uint64

// ID is a Lamport-style logical timestamp that uniquely identifies an Item.
// Only insertion operations increment the Clock; deletions do not.
type ID struct {
	Client ClientID
	Clock  uint64
}

func (a ID) Equals(b ID) bool {
	return a.Client == b.Client && a.Clock == b.Clock
}

// StateVector maps each known ClientID to the highest Clock that has been
// integrated from that client. Everything up to and including that clock
// is known; everything above it is missing.
type StateVector map[ClientID]uint64

// Clock returns the highest integrated clock for a client, or 0 if unknown.
func (sv StateVector) Clock(client ClientID) uint64 {
	return sv[client]
}

// Has reports whether the given ID has already been integrated.
func (sv StateVector) Has(id ID) bool {
	return sv[id.Client] > id.Clock
}

// Clone returns a deep copy.
func (sv StateVector) Clone() StateVector {
	out := make(StateVector, len(sv))
	for k, v := range sv {
		out[k] = v
	}
	return out
}
