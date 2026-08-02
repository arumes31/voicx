# Permission overrides

voicx resolves permissions in this order: server group, client-specific,
channel-specific, channel, channel group, then channel-client. The first
entry for a key wins. The Permission Manager's trace view shows every tier;
the effective-permissions table labels inherited entries with their source.

## Negative and override flags

- `negate` is an explicit denial. It forces the effective value to zero and
  shadows positive entries in lower tiers.
- `skip` locks an entry against lower-tier overrides. Use it only where an
  inherited policy must remain authoritative.
- `b_permission_modify_power_ignore` bypasses normal grant-value caps. It is
  equivalent to administrative permission delegation and should only be held
  by the administrator group.

The conflict detector reports contradictory values that remain stored in
shadowed tiers. A conflict is not nondeterministic—the normal tier order still
wins—but it often indicates stale policy that should be removed.

Guest defaults explicitly negate file-upload and avatar-upload permissions.
Guests may still join public channels, see the users in those channels, read a
normal page of public history, speak, and publish video unless another policy
denies those actions.
