package mcpadapter

// behavior.go — what each tool does, stated at registration and published as
// MCP tool annotations.
//
// S6 of SP-20260912-0001 (CW-20260912-0066).
//
// WHY THIS IS A REQUIRED PARAMETER RATHER THAN AN OPTION. `mcp.NewTool` seeds
// all four annotation hints as non-nil pointers with defaults, so there is no
// "unset" on the wire: a tool that never mentions annotations still publishes
// readOnly=false, destructive=true, idempotent=false, openWorld=true. Every one
// of Tether's tools shipped that identical tuple, including 41 that are pure
// reads.
//
// That default is the MAXIMALLY CAUTIOUS tuple, which is why the bug was
// invisible -- a client reading it over-confirms rather than under-confirms. So
// this work is not "fix 90 wrong values"; it is "make 90 safe-but-uninformative
// values specific, without making any one of them unsafely wrong."
//
// THE CLAIMS ARE ASYMMETRIC, and the whole design follows from that:
//
//	readOnly    unsafe when TRUE and the tool writes   -> a client auto-approves a mutation
//	destructive unsafe when FALSE and the tool destroys -> a client auto-approves an irreversible loss
//	openWorld   not safety-critical; a scope surprise, not a data loss
//
// So Reads must be EARNED and takes a justification; Destroys must be
// enumerated and says what is lost; Writes is the safe middle and needs no
// defense. A tool whose behavior is unclear gets Writes, which is honest and
// also the cautious answer.
//
// WHAT THIS TYPE DOES AND DOES NOT PREVENT. It prevents OMISSION: a new tool
// cannot be registered without stating its behavior, because addTool will not
// compile without the argument and panics at registration on a zero value. It
// does NOT prevent a WRONG value -- a copied annotation satisfies it perfectly.
// Miscopying is covered elsewhere, and only partly:
//
//   - behavior_test.go walks each handler for a.client.X and fails when a tool
//     claims Reads while calling a method classified as a write. That reaches
//     the client-routed tools and not the rest.
//   - the hazard list pins the tools whose NAME reads as a read and which
//     write, so none can be flipped by pattern-matching.
//
// Installing a guard and believing it covers a case it does not is the failure
// this comment exists to prevent, so the reach is stated rather than implied.
//
// idempotentHint is DELIBERATELY NOT SET here and stays at its cautious
// default. Assessing repeat semantics per tool is a second pass of comparable
// size and doing it thinly would be worse than not doing it -- an unassessed
// cautious default is indistinguishable from an assessed one, so docs/mcp.md
// says which of the four carry judgment. Tracked separately.

import mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

type behaviorKind uint8

const (
	// behaviorUnset is the zero value and is never valid. It exists so a
	// Behavior{} literal is detectable at registration rather than silently
	// publishing readOnly=false, destructive=false -- which would be the most
	// permissive tuple of all, the opposite of the safe default.
	behaviorUnset behaviorKind = iota
	behaviorReads
	behaviorWrites
	behaviorDestroys
)

// Behavior is what a tool does. Construct it with Reads, Writes or Destroys;
// the zero value is rejected.
type Behavior struct {
	kind behaviorKind
	// why records what earned the claim. Required for Reads (the store or
	// client read that establishes it) and for Destroys (what is lost).
	why       string
	openWorld bool
}

// Reads declares a tool that does not modify anything.
//
// THE CLAIM MUST BE EARNED, which is what `why` is for: name the read that
// establishes it, not the tool's own name. `mux_message_inbox` is the standing
// counterexample -- its name, its handler's callee, its HTTP verb (a GET) and
// its API handler all read as a read, and four layers down the store does
// UPDATE ... SET delivered_at inside a write transaction. Anything short of
// following the chain is pattern-matching, and that tool defeats every pattern.
func Reads(why string) Behavior {
	return Behavior{kind: behaviorReads, why: why}
}

// Writes declares a tool that modifies state without destroying information.
// Creates, attaches, assignments and sends are writes. It is the safe middle
// and the right answer when the behavior is not established.
func Writes() Behavior {
	return Behavior{kind: behaviorWrites}
}

// Destroys declares a tool that removes or overwrites information that cannot
// be recovered. `why` says what is lost, because "destructive" alone does not
// let an operator judge whether to approve it.
func Destroys(why string) Behavior {
	return Behavior{kind: behaviorDestroys, why: why}
}

// OpenWorld marks a tool that reaches entities outside Tether's own closed
// daemon-and-catalog surface: the MCP proxy, the AI gateway, registry
// federation. Everything served from the local daemon is a closed world.
func (b Behavior) OpenWorld() Behavior {
	b.openWorld = true
	return b
}

// IsReadOnly reports whether this behavior claims the tool modifies nothing.
func (b Behavior) IsReadOnly() bool { return b.kind == behaviorReads }

// valid reports whether the Behavior was constructed rather than zero-valued.
func (b Behavior) valid() bool { return b.kind != behaviorUnset }

// toolAnnotations is the go-mcp-shaped view of a Behavior: the four required
// hint fields go-mcp's server.Tool demands at registration.
type toolAnnotations struct {
	readOnly    bool
	destructive bool
	idempotent  bool
	openWorld   bool
}

// annotations converts the behavior into the MCP hints.
//
// idempotentHint is intentionally absent (stays false, its cautious zero
// value); see the file comment.
func (b Behavior) annotations() toolAnnotations {
	return toolAnnotations{
		readOnly:    b.kind == behaviorReads,
		destructive: b.kind == behaviorDestroys,
		openWorld:   b.openWorld,
	}
}

// sdk converts to the official SDK's own *mcpsdk.ToolAnnotations shape, for
// the handful of tools (mux_call) registered directly against the SDK server
// rather than through go-mcp's RegisterTool -- which already does this
// conversion internally for every tool addTool registers. DestructiveHint
// and OpenWorldHint are always explicit pointers, never left nil: the SDK
// defaults an absent hint to true, which is exactly the silent-assumption
// go-mcp's own required-annotation contract exists to rule out.
func (a toolAnnotations) sdk() *mcpsdk.ToolAnnotations {
	destructive, openWorld := a.destructive, a.openWorld
	return &mcpsdk.ToolAnnotations{
		ReadOnlyHint:    a.readOnly,
		DestructiveHint: &destructive,
		IdempotentHint:  a.idempotent,
		OpenWorldHint:   &openWorld,
	}
}
