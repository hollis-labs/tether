package mcpadapter

// behavior_test.go — the controls behind S6 (CW-20260912-0066), and an explicit
// statement of what each one does NOT cover.
//
// The failure this file exists for is not omission. A required Behavior
// parameter already makes omission a compile error. The failure is a WRONG
// value, and specifically a copied one: annotating a tool `Reads` because the
// tool beside it reads, or because the name looks like a read.
//
// `mux_message_inbox` is why that is not hypothetical. Its name, its handler's
// callee, its HTTP verb (a GET) and its API handler all read as a read, and
// four layers down messagingStore.Inbox runs UPDATE ... SET delivered_at inside
// a write transaction. No signal available at the registration site is
// sufficient, so the check has to reach past the registration site.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// clientMethodWrites classifies every internal/client method reachable from a
// tool handler. TRUE means calling it modifies state.
//
// THIS LIST IS THE POINT. Classifying 40-odd client methods once, where a
// reviewer sees them side by side, is what makes 90 tool annotations
// verifiable — and it is where MessageInbox is marked a write ONCE rather than
// being re-derived at every call site by someone reading its name.
//
// A method reached by a handler and absent here fails the test rather than
// defaulting, so a new client method cannot arrive unclassified.
var clientMethodWrites = map[string]bool{
	// Reads.
	"EventsHistory":              false,
	"StreamEvents":               false,
	"GetWorkstream":              false,
	"ListWorkstreams":            false,
	"ListSessionRefs":            false,
	"ListWorkstreamRefs":         false,
	"SessionDigest":              false,
	"WorkstreamDigest":           false,
	"WorkstreamsForRef":          false,
	"SessionWorkstreamNamespace": false,
	"MessageGet":                 false,
	"MessageList":                false,
	"MessageThread":              false,
	"MessageTrace":               false,
	"MessageRetentionCandidates": false,
	"WaitSession":                false,
	"Whoami":                     false,

	// THE HAZARD. GET /messages/inbox, and messagingStore.Inbox marks every
	// envelope it returns as delivered in the same transaction. Retrying it is
	// not free: the second call consumes a DIFFERENT set of messages, because
	// the first already moved the boundary. CW-20260912-0114 tracks the verb.
	"MessageInbox": true,

	// Writes.
	// POST /messages/{id}/{read,archive,unarchive}; all three mutate the
	// recipient's view of the message. Unlike inbox, the verb is honest.
	"MessageMarkRead":  true,
	"MessageArchive":   true,
	"MessageUnarchive": true,

	"MessageSend":                 true,
	"MessageNotify":               true,
	"MessageConsume":              true,
	"MessageCancel":               true,
	"MessagePurge":                true,
	"MessageRedrive":              true,
	"CreateSession":               true,
	"CreateSessionWithInput":      true,
	"CreateSessionWithBootPrompt": true,
	"LaunchSession":               true,
	"StopSession":                 true,
	"ResizeSession":               true,
	"SendInput":                   true,
	"SendTurn":                    true,
	"ResumeLogicalAgent":          true,
	"CreateWorkstream":            true,
	"AssignSessionWorkstream":     true,
	"EnsureSessionWorkstream":     true,
	"AttachSessionRef":            true,

	// Sub-client accessors. Returning the sub-client modifies nothing; what it
	// is then asked to do is classified in subClientWrites.
	"Bindings":       false,
	"Groups":         false,
	"Registry":       false,
	"ScopedBindings": false,
}

// subClientWrites classifies the methods called ON a sub-client, e.g.
// a.client.Groups().ListMessages(...).
//
// tether_group_read is the near-miss worth recording: it looks exactly like
// mux_message_inbox and is NOT the same, because registry.ListGroupMessages
// contains no write and MarkRead is a separate explicit call. Groups got right
// what messaging did not, and the only way to know that is to have looked.
var subClientWrites = map[string]bool{
	// Reads.
	"ListMessages": false, "Mentions": false, "ListMembers": false,
	"ListForMember": false, "Lookup": false, "Search": false, "LookupBy": false,
	"Current": false, "List": false, "Resolve": false, "Revisions": false,
	"ListForTarget": false, "ResolveSingle": false, "ListRevisions": false,
	// Writes.
	"Create": true, "AddMember": true, "RemoveMember": true, "Leave": true,
	"Send": true, "MarkRead": true, "SetMemberRole": true, "Archive": true,
	"Register": true, "Deregister": true, "UpdateSelf": true, "Sync": true,
	"Merge": true, "Lease": true, "Renew": true, "Revoke": true, "Set": true,
}

type registration struct {
	tool     string
	readOnly bool
	handler  string
	pos      string
}

// parseRegistrations reads this package's own source and returns every
// a.addTool call with the behavior it declares and the handler it names.
func parseRegistrations(t *testing.T) ([]registration, map[string]*ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	// parser.ParseFile over an explicit glob rather than ParseDir, which is
	// deprecated because it ignores build tags. Nothing here is build-tagged,
	// but the lint is the gate and the glob is no harder to read.
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("parsed no source files; this test would pass vacuously")
	}
	handlers := map[string]*ast.FuncDecl{}
	var regs []registration
	{
		for _, f := range files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil {
					handlers[fd.Name.Name] = fd
				}
			}
		}
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				ce, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := ce.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "addTool" || len(ce.Args) < 4 {
					return true
				}
				r := registration{pos: fset.Position(ce.Pos()).String()}
				ast.Inspect(ce.Args[1], func(m ast.Node) bool {
					lit, ok := m.(*ast.BasicLit)
					if ok && lit.Kind == token.STRING && r.tool == "" {
						if v, err := strconv.Unquote(lit.Value); err == nil &&
							(strings.HasPrefix(v, "mux_") || strings.HasPrefix(v, "tether_")) {
							r.tool = v
						}
					}
					return true
				})
				r.readOnly = rootCallName(ce.Args[len(ce.Args)-2]) == "Reads"
				if h, ok := ce.Args[len(ce.Args)-1].(*ast.SelectorExpr); ok {
					r.handler = h.Sel.Name
				}
				regs = append(regs, r)
				return true
			})
		}
	}
	if len(regs) == 0 {
		t.Fatal("no addTool registrations found; the parser is looking in the wrong place")
	}
	return regs, handlers
}

// rootCallName returns the name of the innermost call in a chain, so
// Reads("...").OpenWorld() reports "Reads".
func rootCallName(e ast.Expr) string {
	for {
		ce, ok := e.(*ast.CallExpr)
		if !ok {
			return ""
		}
		switch fn := ce.Fun.(type) {
		case *ast.Ident:
			return fn.Name
		case *ast.SelectorExpr:
			e = fn.X
		default:
			return ""
		}
	}
}

// clientCallsIn returns the client methods a handler reaches, as the terminal
// method name — a.client.Groups().ListMessages(...) yields "ListMessages".
func clientCallsIn(fd *ast.FuncDecl) (direct, sub []string) {
	if fd == nil {
		return nil, nil
	}
	ast.Inspect(fd, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// a.client.X
		if inner, ok := sel.X.(*ast.SelectorExpr); ok {
			if id, ok := inner.X.(*ast.Ident); ok && id.Name == "a" && inner.Sel.Name == "client" {
				direct = append(direct, sel.Sel.Name)
			}
		}
		// a.client.Accessor().X
		if call, ok := sel.X.(*ast.CallExpr); ok {
			if acc, ok := call.Fun.(*ast.SelectorExpr); ok {
				if inner, ok := acc.X.(*ast.SelectorExpr); ok {
					if id, ok := inner.X.(*ast.Ident); ok && id.Name == "a" && inner.Sel.Name == "client" {
						sub = append(sub, sel.Sel.Name)
					}
				}
			}
		}
		return true
	})
	return direct, sub
}

// TestReadOnlyClaimsAgreeWithTheClientMethodsTheyCall is the miscopy control.
//
// It reaches past the registration site to the client method, which is the
// nearest layer where read and write are actually distinguishable. Annotating
// mux_message_inbox as Reads fails here, because MessageInbox is classified a
// write in one reviewed list rather than re-judged from its name.
//
// WHAT IT DOES NOT COVER, stated so nobody reads a green run as more than it
// is: tools that reach the daemon in-process through a.svc, the AI gateway, and
// the proxy tools registered as closures. Those carry no resolvable client
// call, so this test is silent about them and they rest on review alone.
func TestReadOnlyClaimsAgreeWithTheClientMethodsTheyCall(t *testing.T) {
	regs, handlers := parseRegistrations(t)
	checked := 0
	for _, r := range regs {
		direct, sub := clientCallsIn(handlers[r.handler])
		for _, m := range direct {
			writes, known := clientMethodWrites[m]
			if !known {
				t.Errorf("%s (%s): calls client.%s, which is not classified in clientMethodWrites; classify it rather than letting it default", r.tool, r.pos, m)
				continue
			}
			if r.readOnly && writes {
				t.Errorf("%s (%s) is annotated Reads but calls client.%s, which writes", r.tool, r.pos, m)
			}
			checked++
		}
		for _, m := range sub {
			writes, known := subClientWrites[m]
			if !known {
				t.Errorf("%s (%s): calls a sub-client %s, which is not classified in subClientWrites", r.tool, r.pos, m)
				continue
			}
			if r.readOnly && writes {
				t.Errorf("%s (%s) is annotated Reads but calls %s, which writes", r.tool, r.pos, m)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("checked no client calls; the AST walk is not finding them and this test is vacuous")
	}
	t.Logf("verified %d client calls across %d registrations", checked, len(regs))
}

// TestKnownMisleadingToolsAreNotReadOnly pins the tools whose NAME reads as a
// read and which write.
//
// Redundant with the test above for today's entries, and deliberately kept:
// that one is silent about anything without a resolvable client call, and this
// one is the guard that survives a refactor which moves the call behind a
// helper. It is also the list to extend when the next one is found.
func TestKnownMisleadingToolsAreNotReadOnly(t *testing.T) {
	hazards := map[string]string{
		"mux_message_inbox": "GET /messages/inbox, and messagingStore.Inbox marks every envelope it returns as delivered in the same write transaction",
	}
	regs, _ := parseRegistrations(t)
	seen := map[string]bool{}
	for _, r := range regs {
		why, hazard := hazards[r.tool]
		if !hazard {
			continue
		}
		seen[r.tool] = true
		if r.readOnly {
			t.Errorf("%s is annotated Reads. It is not a read: %s", r.tool, why)
		}
	}
	for name := range hazards {
		if !seen[name] {
			t.Errorf("hazard %q is no longer registered; remove it from this list deliberately rather than leaving a guard that matches nothing", name)
		}
	}
}

// TestEveryToolDeclaresABehavior guards the omission case at the level the
// compiler cannot: a registration could still pass a zero-valued Behavior{}.
func TestEveryToolDeclaresABehavior(t *testing.T) {
	var b Behavior
	if b.valid() {
		t.Fatal("the zero Behavior reports valid; addTool's panic guard would never fire")
	}
	for _, ctor := range []Behavior{Reads("x"), Writes(), Destroys("x")} {
		if !ctor.valid() {
			t.Errorf("a constructed Behavior reports invalid: %+v", ctor)
		}
	}
}

// TestAnnotationsAreNotAllOneTuple. The bug this task was filed on was ninety
// tools sharing one tuple; a single tuple is the smell regardless of which
// tuple it is.
func TestAnnotationsAreNotAllOneTuple(t *testing.T) {
	regs, _ := parseRegistrations(t)
	reads, writes := 0, 0
	for _, r := range regs {
		if r.readOnly {
			reads++
		} else {
			writes++
		}
	}
	if reads == 0 || writes == 0 {
		t.Fatalf("every tool is on one side of the read/write line (reads=%d writes=%d); that is the shape of the original defect", reads, writes)
	}
	t.Logf("%d registrations: %d read-only, %d not", len(regs), reads, writes)
}
