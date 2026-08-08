package filetransfer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// ReconcileChannelData removes canonical per-channel directories that have no
// authoritative live channel. It is intended for startup after channels have
// been loaded and the automatic-deletion callback has been installed, but
// before any listener accepts work.
//
// Only direct directory names in canonical positive-decimal form are owned by
// this reconciliation. Asset namespaces and noncanonical names such as 007
// are deliberately preserved.
func (s *Server) ReconcileChannelData(ctx context.Context, liveChannelIDs []int64) (int, error) {
	live := make(map[int64]struct{}, len(liveChannelIDs))
	for _, channelID := range liveChannelIDs {
		if channelID > 0 {
			live[channelID] = struct{}{}
		}
	}

	root, err := s.openBlobRoot()
	if err != nil {
		return 0, fmt.Errorf("opening file root for reconciliation: %w", err)
	}
	dir, err := root.Open(".")
	if err != nil {
		closeErr := root.Close()
		return 0, errors.Join(fmt.Errorf("opening file root directory: %w", err), closeErr)
	}
	entries, readErr := dir.ReadDir(-1)
	dirCloseErr := dir.Close()
	rootCloseErr := root.Close()
	if err := errors.Join(readErr, dirCloseErr, rootCloseErr); err != nil {
		return 0, fmt.Errorf("reading file root for reconciliation: %w", err)
	}

	orphans := make([]int64, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		channelID, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || channelID <= 0 || strconv.FormatInt(channelID, 10) != entry.Name() {
			continue
		}
		if _, ok := live[channelID]; !ok {
			orphans = append(orphans, channelID)
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i] < orphans[j] })

	// Close the capability boundary for every discovered orphan before waiting
	// on any one directory. A slow or failing volume must not leave later
	// orphan IDs able to mint transfers while reconciliation is in progress.
	tombstoned := make(map[int64]struct{}, len(orphans))
	var cleanupErrs []error
	for _, channelID := range orphans {
		if err := s.TombstoneChannelData(channelID); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("tombstoning channel %d: %w", channelID, err))
			continue
		}
		tombstoned[channelID] = struct{}{}
	}

	removed := 0
	for _, channelID := range orphans {
		if _, ok := tombstoned[channelID]; !ok {
			continue
		}
		if err := s.DeleteChannelData(ctx, channelID); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("channel %d: %w", channelID, err))
			continue
		}
		removed++
	}
	return removed, errors.Join(cleanupErrs...)
}
