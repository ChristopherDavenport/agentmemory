package agentmemory_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// TestRunUnderAgentturn wires the block and the tools into the loop
// and drives it with the echo adapter, which calls the tool the
// request forces with the user's text as every required argument. Four
// runs, one per tool, over one store: the block re-rendered in
// BeforeModelCall shows each turn what the store holds, every write is
// a function call in the transcript, and the journal names the session.
func TestRunUnderAgentturn(t *testing.T) {
	ctx := agentmemory.WithSession(context.Background(), "sess-echo")
	store := agentmemory.NewMemStore()
	scopes := []agentmemory.Scope{"user", "project"}
	tools := agentmemory.Tools(store, scopes)
	block, _, err := agentmemory.Render(ctx, store, scopes)
	if err != nil {
		t.Fatal(err)
	}
	// The echo adapter fills name, content, old_text and new_text with
	// the prompt, so the prompt is a name, and the entry's content is
	// its own name.
	const name = "favourite-editor"
	steps := []struct {
		tool   string
		output string // a prefix of the tool's output
		block  string // a line the block shows the model on that run
	}{
		{agentmemory.SaveTool, "Saved user/favourite-editor (16 of 4096 bytes) ", "No entries."},
		{agentmemory.PatchTool, "Patched user/favourite-editor (16 of 4096 bytes) ", "### favourite-editor (16 of 4096 bytes)\n\nfavourite-editor\n"},
		{agentmemory.SearchTool, "1 match in user, project for \"favourite-editor\".\n\nuser/favourite-editor (16 bytes)\nfavourite-editor\n", "Entries: 1 shown, 0 omitted. Block: 197 of 32768 bytes"},
		{agentmemory.ForgetTool, "Forgot user/favourite-editor", "### favourite-editor"},
	}
	for _, step := range steps {
		t.Run(step.tool, func(t *testing.T) {
			var seen string
			cfg := agentturn.Config{
				Model:        &echo.Adapter{},
				Instructions: block + "\n\n" + agentmemory.Usage(),
				Tools:        tools,
				Request:      openresponses.Request{ToolChoice: openresponses.ToolChoiceFunction(step.tool)},
				BeforeModelCall: func(ctx context.Context, req *openresponses.Request) error {
					b, _, err := agentmemory.Render(ctx, store, scopes)
					if err != nil {
						return err
					}
					req.Instructions = b + "\n\n" + agentmemory.Usage()
					if seen == "" {
						seen = req.Instructions
					}
					return nil
				},
			}
			var toolEnds []*agentturn.ToolEnd
			var end *agentturn.RunEnd
			for ev := range agentturn.Run(ctx, nil, openresponses.Items{openresponses.UserText(name)}, cfg) {
				switch e := ev.(type) {
				case *agentturn.ToolEnd:
					toolEnds = append(toolEnds, e)
				case *agentturn.RunEnd:
					end = e
				}
			}
			if end == nil || end.Err != nil {
				t.Fatalf("RunEnd = %+v", end)
			}
			if !strings.HasPrefix(seen, "# Memory\n") || !strings.Contains(seen, step.block) {
				t.Errorf("the block on this run does not show %q:\n%s", step.block, seen)
			}
			if len(toolEnds) != 1 {
				t.Fatalf("tool calls = %d, want 1", len(toolEnds))
			}
			te := toolEnds[0]
			if te.Name != step.tool || te.Err != nil {
				t.Fatalf("ToolEnd = %+v", te)
			}
			// A write's result reaches the loop with the journal record
			// on it, which is what a recorder writes between the
			// dispatch and the output without knowing the type.
			rec, err := agenttool.RecordOf(te.Result.Details)
			if err != nil {
				t.Fatalf("RecordOf: %v", err)
			}
			if step.tool == agentmemory.SearchTool {
				if rec != nil {
					t.Errorf("a search recorded %s", rec.Data)
				}
			} else if rec == nil || rec.NS != agentmemory.WriteNS || !strings.Contains(string(rec.Data), `"tool":"`+step.tool+`"`) {
				t.Errorf("%s recorded %+v", step.tool, rec)
			}
			if got := te.Result.Output.String(); !strings.HasPrefix(got, step.output) {
				t.Errorf("tool output = %q, want prefix %q", got, step.output)
			}
			// The transcript records the call and the output as ordinary
			// items.
			var sawCall, sawOutput bool
			for _, item := range end.Items {
				switch v := item.(type) {
				case *openresponses.FunctionCall:
					sawCall = v.Name == step.tool
				case *openresponses.FunctionCallOutput:
					sawOutput = strings.HasPrefix(v.Output.String(), step.output)
				}
			}
			if !sawCall || !sawOutput {
				t.Errorf("transcript missing the call (%v) or its output (%v)", sawCall, sawOutput)
			}
		})
	}
	// Three writes, each attributed to the session the loop ran under.
	var changes []agentmemory.Change
	for c, err := range store.Journal(ctx, 0) {
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, c)
	}
	if len(changes) != 3 {
		t.Fatalf("journal = %d records, want 3", len(changes))
	}
	for i, c := range changes {
		if c.Session != "sess-echo" || c.Entry.Name != name {
			t.Errorf("record %d = %+v", i, c)
		}
	}
	if !changes[2].Entry.Deleted || changes[1].Prev != changes[0].Entry.Hash {
		t.Errorf("journal is not a chain ending in a tombstone: %+v", changes)
	}
	// The definitions the model was offered are the four tools.
	if defs := agenttool.Set(tools).Definitions(); len(defs) != 4 {
		t.Errorf("definitions = %d", len(defs))
	}
}
