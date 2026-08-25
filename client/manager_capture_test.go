package main

import (
	"net"
	"testing"
	"time"

	"voicx/internal/netproto"
)

func TestBindingsRequireManagerOffline(t *testing.T) {
	a := &App{}
	if got := a.FileDelete(1, "", "file"); got != "not connected" {
		t.Fatalf("FileDelete offline = %q", got)
	}
	if _, err := a.FileList(1, ""); err == nil || err.Error() != "not connected" {
		t.Fatalf("FileList offline error = %v", err)
	}
	if got := a.ChatDeleteMessage(1); got != "not connected" {
		t.Fatalf("ChatDeleteMessage offline = %q", got)
	}
	if _, err := a.GetPermissions(); err == nil || err.Error() != "not connected" {
		t.Fatalf("GetPermissions offline error = %v", err)
	}
	if got := a.SetStatus("online", ""); got != "not connected" {
		t.Fatalf("SetStatus offline = %q", got)
	}
}

func TestFileBindingStaysWithCapturedManagerAcrossTabSwitch(t *testing.T) {
	clientA, serverA := net.Pipe()
	clientB, serverB := net.Pipe()
	t.Cleanup(func() {
		_ = clientA.Close()
		_ = serverA.Close()
		_ = clientB.Close()
		_ = serverB.Close()
	})
	cmA, cmB := newTestConnManager(), newTestConnManager()
	cmA.mu.Lock()
	cmA.conn = clientA
	cmA.mu.Unlock()
	cmB.mu.Lock()
	cmB.conn = clientB
	cmB.mu.Unlock()
	go cmA.readLoop(clientA)
	go cmB.readLoop(clientB)
	a := appWithCM(cmA)

	done := make(chan error, 1)
	go func() {
		_, err := a.FileList(7, "")
		done <- err
	}()
	request := readFrame(t, serverA)
	if got := netproto.MessageType(request.Type); got != netproto.MsgFileList {
		t.Fatalf("request sent to A = %v", got)
	}
	a.cmStore(cmB)
	if err := netproto.WriteFrame(serverA, mustEncode(netproto.MsgFileListResponse, netproto.FileListResponse{})); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FileList on captured manager: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("captured FileList did not complete")
	}
	_ = serverB.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
	if _, err := netproto.ReadFrame(serverB); err == nil {
		t.Fatal("tab switch redirected FileList request to manager B")
	}
}

func TestChatSearchKeepsOriginalManagerAcrossPagination(t *testing.T) {
	clientA, serverA := net.Pipe()
	clientB, serverB := net.Pipe()
	t.Cleanup(func() {
		_ = clientA.Close()
		_ = serverA.Close()
		_ = clientB.Close()
		_ = serverB.Close()
	})
	cmA, cmB := newTestConnManager(), newTestConnManager()
	progressA, progressB := &recordingSink{}, &recordingSink{}
	cmA.sink, cmB.sink = progressA, progressB
	cmA.mu.Lock()
	cmA.conn = clientA
	cmA.mu.Unlock()
	cmB.mu.Lock()
	cmB.conn = clientB
	cmB.mu.Unlock()
	go cmA.readLoop(clientA)
	go cmB.readLoop(clientB)
	a := appWithCM(cmA)

	done := make(chan error, 1)
	go func() {
		_, err := a.ChatSearch(7, "", chatSearchPage+1)
		done <- err
	}()
	first := readFrame(t, serverA)
	if netproto.MessageType(first.Type) != netproto.MsgChatHistory {
		t.Fatalf("first page manager A type = %v", netproto.MessageType(first.Type))
	}
	// Switch before delivering the first page. A buggy pager re-resolves cm
	// for page two and would now send the continuation to B.
	a.cmStore(cmB)
	entries := make([]netproto.ChatHistoryEntry, chatSearchPage)
	for i := range entries {
		entries[i] = netproto.ChatHistoryEntry{ID: int64(chatSearchPage - i), Deleted: true}
	}
	if err := netproto.WriteFrame(serverA, mustEncode(netproto.MsgChatHistoryResponse,
		netproto.ChatHistoryResponse{ChannelID: 7, Messages: entries})); err != nil {
		t.Fatal(err)
	}
	second := readFrame(t, serverA)
	if netproto.MessageType(second.Type) != netproto.MsgChatHistory {
		t.Fatalf("second page was not sent to original manager: %v", netproto.MessageType(second.Type))
	}
	if err := netproto.WriteFrame(serverA, mustEncode(netproto.MsgChatHistoryResponse,
		netproto.ChatHistoryResponse{ChannelID: 7})); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("chat scan failed: %v", err)
	}
	if progressA.count("chatsearch:progress") != 1 || progressB.count("chatsearch:progress") != 0 {
		t.Fatalf("search progress routed A/B = %d/%d", progressA.count("chatsearch:progress"), progressB.count("chatsearch:progress"))
	}
	_ = serverB.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
	if _, err := netproto.ReadFrame(serverB); err == nil {
		t.Fatal("active-tab switch redirected second history page to manager B")
	}
}
