package ownership

// VarState tracks the ownership state of a variable.
type VarState int

const (
	Owned    VarState = iota // variable currently owns its value
	Moved                    // value has been moved; further use is invalid
	Borrowed                 // T0338: non-~ non-& non-Copy parameter; reads OK, moves rejected
)

// StateMap maps variable names to their ownership states.
type StateMap map[string]VarState

// clone returns a deep copy of the state map for branching.
func (s StateMap) clone() StateMap {
	c := make(StateMap, len(s))
	for k, v := range s {
		c[k] = v
	}
	return c
}

// merge performs conservative merge: if either branch has Moved, result is Moved.
// Borrowed is a fixed point — borrowed parameters stay borrowed across branches.
func merge(a, b StateMap) StateMap {
	result := make(StateMap, len(a))
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			result[k] = va
			continue
		}
		if va == Moved || vb == Moved {
			result[k] = Moved
		} else if va == Borrowed || vb == Borrowed {
			result[k] = Borrowed
		} else {
			result[k] = Owned
		}
	}
	for k, vb := range b {
		if _, ok := result[k]; !ok {
			result[k] = vb
		}
	}
	return result
}
