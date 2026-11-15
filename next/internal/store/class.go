package store

import (
	"fmt"
	"sort"
)

// Class is a table's storage class (plan section 13.2). Each class is reached through its own Handle.
// At the cutover all three handles share one store; a class gets its own store only when it moves.
type Class int

const (
	// A is small and local and always boots: boot state, critical config, definitions, settings, and what
	// auth needs to answer a request. It stays on local SQLite.
	A Class = iota + 1
	// B is events, logs and telemetry. It moves to Postgres first.
	B
	// C is domain data. It moves to Postgres later.
	C
)

func (c Class) String() string {
	switch c {
	case A:
		return "A"
	case B:
		return "B"
	case C:
		return "C"
	}
	return fmt.Sprintf("Class(%d)", int(c))
}

// ClassOf returns a table's class, and false for a table with none.
func ClassOf(table string) (Class, bool) {
	c, ok := tableClass[table]
	return c, ok
}

// ClassedTables returns every table that has a class, sorted.
func ClassedTables() []string {
	out := make([]string, 0, len(tableClass))
	for t := range tableClass {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
