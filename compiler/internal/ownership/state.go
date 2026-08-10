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
//
// T1381: for a must-use variable (one that transitively owns a `failable_task[T]`),
// the merge is INVERTED. A must-use value is *discharged* (received or moved
// onward) only when that happened on EVERY path — dropping it on any path
// silently swallows its error (§17.2.1). So a must-use name is Moved after the
// merge only if BOTH branches have it Moved; otherwise it stays Owned
// (undischarged on at least one path) and is reported at scope end. This is the
// dual of the normal Moved-absorbing rule. Diverging branches are already
// excluded from the merge by the callers (checkIfStmt/checkSelectStmt/loops),
// so the merge only sees fall-through paths.
func (c *Checker) merge(a, b StateMap) StateMap {
	result := make(StateMap, len(a))
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			result[k] = va
			continue
		}
		if _, mustUse := c.mustUse[k]; mustUse {
			switch {
			case va == Moved && vb == Moved:
				result[k] = Moved
			case va == Borrowed || vb == Borrowed:
				result[k] = Borrowed
			default:
				result[k] = Owned
			}
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
