// files.go implements the file-transfer control handlers of the TCP control
// server: issuing transfer tokens, listing/managing channel files (folders,
// rename, delete, versions), and minting download links. The actual byte
// transfer happens on the file-transfer port (internal/filetransfer); these
// handlers only do permission checks and bookkeeping.
package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/store"
)

// handleFileTransferInit issues a single-use transfer token after a
// permission check (upload/download power; unset = allowed, negated =
// denied) and the file-transfer backend's own validation (size cap, quota,
// name/folder sanitization).
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
		transferID, token, err = s.deps.FileTransfer.InitUpload(ctx, msg.ChannelID, msg.Folder, msg.Name, msg.Size, client.UniqueID)
	case "download":
		if !pc.ftDownloadAllowed() {
			return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyFTFileDownloadPower))
		}
		transferID, token, err = s.deps.FileTransfer.InitDownload(ctx, msg.ChannelID, msg.Folder, msg.Name)
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

// fileEntries maps store records to wire entries.
func fileEntries(files []store.FileRecord) []netproto.FileEntry {
	out := make([]netproto.FileEntry, 0, len(files))
	for _, rec := range files {
		out = append(out, netproto.FileEntry{
			Name:       rec.Name,
			Folder:     rec.Folder,
			Size:       rec.Size,
			SHA256:     rec.SHA256,
			Uploader:   rec.Uploader,
			UploadedAt: rec.UploadedAt,
		})
	}
	return out
}

// handleFileList returns the channel's file listing for one folder plus the
// channel quota state (265). Listing requires the download permission.
func (s *TCPServer) handleFileList(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.FileList
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed file_list: "+err.Error())
	}
	if s.deps == nil || s.deps.FileTransfer == nil {
		return s.sendError(client, errCodeUnavailable, "file transfer backend unavailable")
	}
	folder := msg.Folder
	if folder == "" && msg.Path != "" && msg.Path != "/" {
		return s.sendError(client, errCodeMalformed, "legacy path field supports only empty or /")
	}

	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.ftDownloadAllowed() {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyFTFileDownloadPower))
	}

	files, err := s.deps.FileTransfer.ListFiles(ctx, msg.ChannelID, folder)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "listing files failed")
	}
	folders, err := s.deps.FileTransfer.ListFileFolders(ctx, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "listing folders failed")
	}
	used, quota, err := s.deps.FileTransfer.ChannelQuota(ctx, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "reading quota failed")
	}

	return s.writeMessage(client, netproto.MsgFileListResponse, netproto.FileListResponse{
		Entries:    fileEntries(files),
		Folders:    folders,
		UsedBytes:  used,
		QuotaBytes: quota,
	})
}

// fileManageAllowed gates file delete/rename (263): the uploader and holders
// of b_ft_delete (admins bypass) may manage a file.
func (s *TCPServer) fileManageAllowed(ctx context.Context, client *Client, channelID int64, folder, name string) error {
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return fmt.Errorf("permission backend unavailable")
	}
	if pc.admin || pc.granted(permissions.PermissionKeyFTFileDelete) {
		return nil
	}
	files, err := s.deps.FileTransfer.ListFiles(ctx, channelID, folder)
	if err != nil {
		return fmt.Errorf("checking file owner failed")
	}
	for _, rec := range files {
		if rec.Name == name {
			if rec.Uploader == client.UniqueID {
				return nil
			}
			return fmt.Errorf("insufficient permission: only the uploader or %s holders may manage this file",
				permissions.PermissionKeyFTFileDelete)
		}
	}
	return fmt.Errorf("file not found")
}

// handleFileDelete deletes a channel file (record + blob), audited.
func (s *TCPServer) handleFileDelete(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.FileDelete
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed file_delete: "+err.Error())
	}
	if s.deps == nil || s.deps.FileTransfer == nil {
		return s.sendError(client, errCodeUnavailable, "file transfer backend unavailable")
	}
	if err := s.fileManageAllowed(ctx, client, msg.ChannelID, msg.Folder, msg.Name); err != nil {
		return s.sendError(client, errCodePermissionDenied, err.Error())
	}
	if err := s.deps.FileTransfer.DeleteFile(ctx, msg.ChannelID, msg.Folder, msg.Name); err != nil {
		return s.sendError(client, errCodeNotFound, "delete failed: "+err.Error())
	}
	s.audit(ctx, client.UniqueID, "file_delete", fmt.Sprintf("%d:%s/%s", msg.ChannelID, msg.Folder, msg.Name), "")
	return nil
}

// handleFileRename renames/moves a channel file (record + blob), audited.
func (s *TCPServer) handleFileRename(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.FileRename
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed file_rename: "+err.Error())
	}
	if s.deps == nil || s.deps.FileTransfer == nil {
		return s.sendError(client, errCodeUnavailable, "file transfer backend unavailable")
	}
	if msg.NewName == "" {
		return s.sendError(client, errCodeMalformed, "new_name is required")
	}
	if err := s.fileManageAllowed(ctx, client, msg.ChannelID, msg.Folder, msg.Name); err != nil {
		return s.sendError(client, errCodePermissionDenied, err.Error())
	}
	if err := s.deps.FileTransfer.RenameFile(ctx, msg.ChannelID, msg.Folder, msg.Name, msg.NewFolder, msg.NewName); err != nil {
		return s.sendError(client, errCodeNotFound, "rename failed: "+err.Error())
	}
	s.audit(ctx, client.UniqueID, "file_rename", fmt.Sprintf("%d:%s/%s", msg.ChannelID, msg.Folder, msg.Name),
		fmt.Sprintf("to %s/%s", msg.NewFolder, msg.NewName))
	return nil
}

// handleFileVersions lists a file's rotated old versions (264).
func (s *TCPServer) handleFileVersions(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.FileVersions
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed file_versions: "+err.Error())
	}
	if s.deps == nil || s.deps.FileTransfer == nil {
		return s.sendError(client, errCodeUnavailable, "file transfer backend unavailable")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.ftDownloadAllowed() {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyFTFileDownloadPower))
	}
	files, err := s.deps.FileTransfer.ListFileVersions(ctx, msg.ChannelID, msg.Folder, msg.Name)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "listing versions failed")
	}
	return s.writeMessage(client, netproto.MsgFileVersionsResponse, netproto.FileVersionsResponse{
		Entries: fileEntries(files),
	})
}

// handleFileLink mints an expiring download link (267). Gated by admin or
// the file's uploader (links bypass auth on the HTTP path, so issuance is
// restricted).
func (s *TCPServer) handleFileLink(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.FileLink
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed file_link: "+err.Error())
	}
	if s.deps == nil || s.deps.FileTransfer == nil {
		return s.sendError(client, errCodeUnavailable, "file transfer backend unavailable")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.admin {
		files, err := s.deps.FileTransfer.ListFiles(ctx, msg.ChannelID, msg.Folder)
		if err != nil {
			return s.sendError(client, errCodeUnavailable, "checking file owner failed")
		}
		owner := false
		for _, rec := range files {
			if rec.Name == msg.Name && rec.Uploader == client.UniqueID {
				owner = true
			}
		}
		if !owner {
			return s.sendError(client, errCodePermissionDenied, "only the uploader or an admin may create download links")
		}
	}
	token, expires, err := s.deps.FileTransfer.CreateLink(ctx, msg.ChannelID, msg.Folder, msg.Name)
	if err != nil {
		return s.sendError(client, errCodeNotFound, "link failed: "+err.Error())
	}
	s.audit(ctx, client.UniqueID, "file_link", fmt.Sprintf("%d:%s/%s", msg.ChannelID, msg.Folder, msg.Name), "")

	// The client builds the final URL from its own control host (the server
	// cannot know its published address behind Docker/NAT); the health port
	// is where the /dl handler lives.
	port := s.cfg.HealthAddr
	if i := strings.LastIndex(port, ":"); i >= 0 {
		port = port[i+1:]
	}
	portNum, _ := strconv.Atoi(port)
	return s.writeMessage(client, netproto.MsgFileLinkResponse, netproto.FileLinkResponse{
		Path:       "/dl/" + token,
		HealthPort: portNum,
		ExpiresAt:  expires.Unix(),
	})
}
