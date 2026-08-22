// Scoped actions capture an immutable UI identity before awaiting a modal or
// bridge call. They prevent a delayed confirmation from mutating a different
// channel, folder, tab, or server after the user has navigated away.
export function captureScope(readScope) {
    return Object.freeze({ ...readScope() });
}

export function scopeIsCurrent(scope, readScope) {
    const current = readScope();
    const keys = Object.keys(scope);
    return keys.length === Object.keys(current).length &&
        keys.every((key) => current[key] === scope[key]);
}

export async function runScopedDialogAction({ readScope, openDialog, isAccepted, perform }) {
    const scope = captureScope(readScope);
    const value = await openDialog(scope);
    if (!isAccepted(value) || !scopeIsCurrent(scope, readScope)) {
        return { performed: false, scope, value };
    }
    await perform(scope, value);
    return { performed: true, scope, value };
}
