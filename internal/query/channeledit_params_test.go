// channeledit_params_test.go covers the ServerQuery channeledit arguments
// added for the tree-editing fields (157/160/163/168).
package query

import (
	"context"
	"strings"
	"testing"
)

// recordingBackend captures the ChannelEditParams the command built.
type recordingBackend struct {
	*fakeBackend
	last ChannelEditParams
}

func (r *recordingBackend) EditChannel(ctx context.Context, channelID int64, params ChannelEditParams) error {
	r.last = params
	return r.fakeBackend.EditChannel(ctx, channelID, params)
}

// TestChanneleditTreeParams verifies the join power, order index, parent and
// inheritance arguments reach the backend.
func TestChanneleditTreeParams(t *testing.T) {
	be := &recordingBackend{fakeBackend: newFakeBackend()}
	addr, _ := startQueryServer(t, be)
	conn, r := dialQuery(t, addr)
	defer conn.Close()
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r,
		"channeledit cid=1 channel_needed_join_power=75 channel_order=3 cpid=9 channel_inherit_permissions=1")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("channeledit = %q", got)
	}

	if be.last.NeededJoinPower == nil || *be.last.NeededJoinPower != 75 {
		t.Errorf("NeededJoinPower = %v, want 75", be.last.NeededJoinPower)
	}
	if be.last.OrderIndex == nil || *be.last.OrderIndex != 3 {
		t.Errorf("OrderIndex = %v, want 3", be.last.OrderIndex)
	}
	if be.last.ParentID == nil || *be.last.ParentID != 9 {
		t.Errorf("ParentID = %v, want 9", be.last.ParentID)
	}
	if be.last.InheritPermissions == nil || !*be.last.InheritPermissions {
		t.Errorf("InheritPermissions = %v, want true", be.last.InheritPermissions)
	}
}

// TestChanneleditTreeParamsOmitted verifies the new fields stay nil when the
// arguments are absent, so an edit of one field cannot reset the others.
func TestChanneleditTreeParamsOmitted(t *testing.T) {
	be := &recordingBackend{fakeBackend: newFakeBackend()}
	addr, _ := startQueryServer(t, be)
	conn, r := dialQuery(t, addr)
	defer conn.Close()
	loginOK(t, conn, r)

	lines := sendCmd(t, conn, r, "channeledit cid=1 channel_topic=hi")
	if got := lastErr(t, lines); got != "error id=0 msg=ok" {
		t.Fatalf("channeledit = %q", got)
	}
	if be.last.NeededJoinPower != nil || be.last.OrderIndex != nil ||
		be.last.ParentID != nil || be.last.InheritPermissions != nil {
		t.Errorf("omitted fields must stay nil, got %+v", be.last)
	}
}

// TestChanneleditTreeParamsInvalid verifies each new argument is validated
// rather than silently dropped.
func TestChanneleditTreeParamsInvalid(t *testing.T) {
	be := &recordingBackend{fakeBackend: newFakeBackend()}
	addr, _ := startQueryServer(t, be)
	conn, r := dialQuery(t, addr)
	defer conn.Close()
	loginOK(t, conn, r)

	for _, arg := range []string{
		"channel_needed_join_power=abc",
		"channel_order=abc",
		"cpid=abc",
		"channel_inherit_permissions=maybe",
	} {
		lines := sendCmd(t, conn, r, "channeledit cid=1 "+arg)
		if got := lastErr(t, lines); !strings.HasPrefix(got, "error id=512") {
			t.Errorf("channeledit %s = %q, want an invalid-parameter error", arg, got)
		}
	}
}

// TestHelpListsChanneleditTreeParams keeps the advertised usage in step with
// what the command actually parses.
func TestHelpListsChanneleditTreeParams(t *testing.T) {
	addr, _ := startQueryServer(t, newFakeBackend())
	conn, r := dialQuery(t, addr)
	defer conn.Close()

	lines := sendCmd(t, conn, r, "help")
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"channel_needed_join_power", "channel_order", "cpid",
		"channel_inherit_permissions",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("help output does not mention %q", want)
		}
	}
}
