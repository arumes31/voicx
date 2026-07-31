// files.go implements the file-transfer control handlers of the TCP control
// server: issuing transfer tokens and listing channel files. The actual byte
// transfer happens on the file-transfer port (internal/filetransfer); these
// handlers only do permission checks and token issuance.
package server

import (
	"context"

	"go.uber.org/zap"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
)

// handleFileTransferInit issues a single-use transfer token after a
// permission check (upload/download power; unset = allowed, negated =
// denied) and the file-transfer backend's own validation (size cap, quota,
// name sanitization).
func (s *TCPServer) handleFileTransferInit(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.FileTransferInit
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed file_transfer_init: "+err.Error())
	}
	if s.deps == nil || s.deps.FileTransfer == nil {
		return s.sendError(client, errCodeUnavailable, "file transfer backend unavailable")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}

	var transferID, token string
	switch msg.Direction {
	case "upload":
		if !pc.ftUploadAllowed() {
			return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyFTFileUploadPower))
		}
		transferID, token, err = s.deps.FileTransfer.InitUpload(ctx, msg.ChannelID, msg.Name, msg.Size, client.UniqueID)
	case "download":
		if !pc.ftDownloadAllowed() {
			return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyFTFileDownloadPower))
		}
		transferID, token, err = s.deps.FileTransfer.InitDownload(ctx, msg.ChannelID, msg.Name)
	default:
		return s.sendError(client, errCodeMalformed, "direction must be upload or download")
	}
	if err != nil {
		return s.sendError(client, errCodeMalformed, "file transfer init failed: "+err.Error())
	}

	s.logger.Info("file transfer token issued",
		zap.String("client_id", client.ID),
		zap.String("direction", msg.Direction),
		zap.Int64("channel_id", msg.ChannelID),
		zap.String("name", msg.Name),
	)
	return s.writeMessage(client, netproto.MsgFileTransferInitResponse, netproto.FileTransferInitResponse{
		TransferID: transferID,
		Token:      token,
		Port:       s.deps.FileTransfer.Port(),
	})
}

// handleFileList returns the channel's file listing. Listing requires the
// download permission. The Path field is reserved for future subdirectory
// support and must be empty or "/".
func (s *TCPServer) handleFileList(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.FileList
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed file_list: "+err.Error())
	}
	if s.deps == nil || s.deps.FileTransfer == nil {
		return s.sendError(client, errCodeUnavailable, "file transfer backend unavailable")
	}
	if msg.Path != "" && msg.Path != "/" {
		return s.sendError(client, errCodeMalformed, "subdirectories are not supported")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.ftDownloadAllowed() {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyFTFileDownloadPower))
	}

	files, err := s.deps.FileTransfer.ListFiles(ctx, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "listing files failed")
	}

	resp := netproto.FileListResponse{Entries: make([]netproto.FileEntry, 0, len(files))}
	for _, rec := range files {
		resp.Entries = append(resp.Entries, netproto.FileEntry{
			Name:       rec.Name,
			Size:       rec.Size,
			SHA256:     rec.SHA256,
			Uploader:   rec.Uploader,
			UploadedAt: rec.UploadedAt,
		})
	}
	return s.writeMessage(client, netproto.MsgFileListResponse, resp)
}
