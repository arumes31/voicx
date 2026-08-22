package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSettingsDeepCopiesAtEveryBoundary(t *testing.T) {
	a := &App{settings: DefaultSettings(), settingsPath: filepath.Join(t.TempDir(), "settings.json")}
	a.settings.Bookmarks = []Bookmark{{Name: "home", Addr: "one"}}
	a.settings.UserVolumes = map[string]int{"alice": 100}
	a.settings.Contacts = []Contact{{UniqueID: "alice", NickHistory: []string{"old"}}}
	a.settings.Keywords = map[string][]string{"one": {"hello"}}

	got := a.GetSettings()
	got.Bookmarks[0].Addr = "mutated"
	got.UserVolumes["alice"] = 1
	got.Contacts[0].NickHistory[0] = "mutated"
	got.Keywords["one"][0] = "mutated"
	again := a.GetSettings()
	if again.Bookmarks[0].Addr != "one" || again.UserVolumes["alice"] != 100 ||
		again.Contacts[0].NickHistory[0] != "old" || again.Keywords["one"][0] != "hello" {
		t.Fatalf("GetSettings shared nested data: %+v", again)
	}

	incoming := a.GetSettings()
	incoming.UserVolumes["bob"] = 80
	incoming.Contacts[0].NickHistory = append(incoming.Contacts[0].NickHistory, "new")
	if err := a.SaveSettings(incoming); err != "" {
		t.Fatal(err)
	}
	incoming.UserVolumes["bob"] = 1
	incoming.Contacts[0].NickHistory[1] = "mutated"
	persisted := a.GetSettings()
	if persisted.UserVolumes["bob"] != 80 || persisted.Contacts[0].NickHistory[1] != "new" {
		t.Fatalf("SaveSettings retained caller aliases: %+v", persisted)
	}
}

func TestSettingsSideEffectsConvergeAfterSerializedTransactions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	a := &App{settings: DefaultSettings(), settingsPath: path, hotkeys: map[string]*hotkeyReg{}}
	originalWriter := settingsSnapshotWriter
	originalOpacity := windowOpacityApply
	originalHotkeys := settingsHotkeyApplier
	t.Cleanup(func() {
		settingsSnapshotWriter = originalWriter
		windowOpacityApply = originalOpacity
		settingsHotkeyApplier = originalHotkeys
	})
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	settingsSnapshotWriter = func(path string, s Settings) error {
		if (s.WindowOpacity == 30 || s.HotkeyPTT == "Ctrl+A") && once.CompareAndSwap(false, true) {
			close(blocked)
			<-release
		}
		return originalWriter(path, s)
	}
	var mu sync.Mutex
	var opacity []int
	var ptt []string
	windowOpacityApply = func(value int) error {
		mu.Lock()
		opacity = append(opacity, value)
		mu.Unlock()
		return nil
	}
	settingsHotkeyApplier = func(_ *App, action, spec string) {
		if action == "ptt" {
			mu.Lock()
			ptt = append(ptt, spec)
			mu.Unlock()
		}
	}

	oldOpacity := make(chan string, 1)
	go func() { oldOpacity <- a.SetWindowOpacity(30) }()
	<-blocked
	newOpacity := make(chan string, 1)
	go func() { newOpacity <- a.SetWindowOpacity(80) }()
	close(release)
	if got := <-oldOpacity; got != "" {
		t.Fatal(got)
	}
	if got := <-newOpacity; got != "" {
		t.Fatal(got)
	}
	mu.Lock()
	gotOpacity := append([]int(nil), opacity...)
	mu.Unlock()
	if len(gotOpacity) == 0 || gotOpacity[len(gotOpacity)-1] != 80 {
		t.Fatalf("final opacity side effect %v, want 80", gotOpacity)
	}
	if got := loadSettingsAt(path).WindowOpacity; got != 80 {
		t.Fatalf("disk opacity = %d, want 80", got)
	}

	// Repeat the same ordering through a hotkey setter. The old caller may
	// finish first, but it must apply the newer PTT spec rather than Ctrl+A.
	blocked = make(chan struct{})
	release = make(chan struct{})
	once.Store(false)
	oldHotkey := make(chan string, 1)
	go func() { oldHotkey <- a.SetHotkey("ptt", "Ctrl+A") }()
	<-blocked
	newHotkey := make(chan string, 1)
	go func() { newHotkey <- a.SetHotkey("ptt", "Ctrl+B") }()
	close(release)
	if got := <-oldHotkey; got != "" {
		t.Fatal(got)
	}
	if got := <-newHotkey; got != "" {
		t.Fatal(got)
	}
	mu.Lock()
	gotPTT := append([]string(nil), ptt...)
	mu.Unlock()
	if len(gotPTT) == 0 || gotPTT[len(gotPTT)-1] != "Ctrl+B" {
		t.Fatalf("final hotkey side effect %v, want Ctrl+B", gotPTT)
	}
	if got := loadSettingsAt(path).HotkeyPTT; got != "Ctrl+B" {
		t.Fatalf("disk PTT = %q, want Ctrl+B", got)
	}
}

func TestStaleSettingsEffectCannotOverwriteNewerLiveEffect(t *testing.T) {
	a := &App{
		ctx:          context.Background(),
		settings:     DefaultSettings(),
		settingsPath: filepath.Join(t.TempDir(), "settings.json"),
		hotkeys:      map[string]*hotkeyReg{},
		eventEmit:    func(string, any) {},
	}
	originalOpacity := windowOpacityApply
	originalHotkeys := settingsHotkeyApplier
	originalAOT := alwaysOnTopApply
	t.Cleanup(func() {
		windowOpacityApply = originalOpacity
		settingsHotkeyApplier = originalHotkeys
		alwaysOnTopApply = originalAOT
	})

	blockTicket := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	a.beforeSettingsEffect = func(_ uint64, _ Settings) {
		if once.CompareAndSwap(false, true) {
			close(blockTicket)
			<-release
		}
	}
	var mu sync.Mutex
	var opacity []int
	var ptt []string
	var aot []bool
	windowOpacityApply = func(value int) error {
		mu.Lock()
		opacity = append(opacity, value)
		mu.Unlock()
		return nil
	}
	settingsHotkeyApplier = func(_ *App, action, spec string) {
		if action == "ptt" {
			mu.Lock()
			ptt = append(ptt, spec)
			mu.Unlock()
		}
	}
	alwaysOnTopApply = func(_ context.Context, value bool) {
		mu.Lock()
		aot = append(aot, value)
		mu.Unlock()
	}

	old := make(chan string, 1)
	go func() { old <- a.SetWindowOpacity(30) }()
	<-blockTicket // old effect has observed its snapshot but cannot commit it.
	if got := a.SetWindowOpacity(80); got != "" {
		t.Fatal(got)
	}
	close(release)
	if got := <-old; got != "" {
		t.Fatal(got)
	}

	// Repeat for the other live effect families; their final observable value
	// must be the newer ticket even when the stale one resumes last.
	once.Store(false)
	blockTicket, release = make(chan struct{}), make(chan struct{})
	old = make(chan string, 1)
	go func() { old <- a.SetHotkey("ptt", "Ctrl+A") }()
	<-blockTicket
	if got := a.SetHotkey("ptt", "Ctrl+B"); got != "" {
		t.Fatal(got)
	}
	close(release)
	if got := <-old; got != "" {
		t.Fatal(got)
	}

	once.Store(false)
	blockTicket, release = make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() { a.SetAlwaysOnTop(false); close(done) }()
	<-blockTicket
	a.SetAlwaysOnTop(true)
	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	for _, value := range opacity {
		if value != 80 {
			t.Fatalf("stale opacity effect: %v", opacity)
		}
	}
	for _, spec := range ptt {
		if spec != "Ctrl+B" {
			t.Fatalf("stale hotkey effect: %v", ptt)
		}
	}
	for _, value := range aot {
		if !value {
			t.Fatalf("stale always-on-top effect: %v", aot)
		}
	}
}

func TestSettingsEffectsKeepIndependentFamilyCommits(t *testing.T) {
	type family struct {
		name    string
		old     func(*App) string
		new     func(*App) string
		live    func(*effectRecorder) any
		wantOld any
		wantNew any
		value   func(Settings) any
	}
	makeFamilies := func() []family {
		return []family{
			{
				name:    "opacity",
				old:     func(a *App) string { return a.SetWindowOpacity(31) },
				new:     func(a *App) string { return a.SetWindowOpacity(82) },
				live:    func(r *effectRecorder) any { return r.lastOpacity() },
				wantOld: 31, wantNew: 82,
				value: func(s Settings) any { return s.WindowOpacity },
			},
			{
				name:    "hotkey",
				old:     func(a *App) string { return a.SetHotkey("ptt", "Ctrl+A") },
				new:     func(a *App) string { return a.SetHotkey("ptt", "Ctrl+B") },
				live:    func(r *effectRecorder) any { return r.lastPTT() },
				wantOld: "Ctrl+A", wantNew: "Ctrl+B",
				value: func(s Settings) any { return s.HotkeyPTT },
			},
			{
				name:    "always_on_top",
				old:     func(a *App) string { a.SetAlwaysOnTop(false); return "" },
				new:     func(a *App) string { a.SetAlwaysOnTop(true); return "" },
				live:    func(r *effectRecorder) any { return r.lastAOT() },
				wantOld: false, wantNew: true,
				value: func(s Settings) any { return s.AlwaysOnTop },
			},
		}
	}

	for _, old := range makeFamilies() {
		for _, newer := range makeFamilies() {
			t.Run(old.name+"_then_"+newer.name, func(t *testing.T) {
				a, recorder := newEffectTestApp(t)
				entered, release := make(chan struct{}), make(chan struct{})
				var once atomic.Bool
				a.beforeSettingsEffect = func(_ uint64, _ Settings) {
					if once.CompareAndSwap(false, true) {
						close(entered)
						<-release
					}
				}
				oldDone := make(chan string, 1)
				go func() { oldDone <- old.old(a) }()
				<-entered // old family has observed a snapshot but has not committed.
				if got := newer.new(a); got != "" {
					t.Fatal(got)
				}
				close(release)
				if got := <-oldDone; got != "" {
					t.Fatal(got)
				}

				wantOld := old.wantOld
				if old.name == newer.name {
					wantOld = newer.wantNew
				}
				if got := old.live(recorder); got != wantOld {
					t.Fatalf("%s live = %v, want %v", old.name, got, wantOld)
				}
				if got := newer.live(recorder); got != newer.wantNew {
					t.Fatalf("%s live = %v, want %v", newer.name, got, newer.wantNew)
				}
				memory := a.GetSettings()
				disk := loadSettingsAt(a.settingsPath)
				for _, check := range []struct {
					family family
					want   any
				}{{old, wantOld}, {newer, newer.wantNew}} {
					if got := check.family.value(memory); got != check.want {
						t.Fatalf("%s memory = %v, want %v", check.family.name, got, check.want)
					}
					if got := check.family.value(disk); got != check.want {
						t.Fatalf("%s disk = %v, want %v", check.family.name, got, check.want)
					}
				}
			})
		}
	}
}

func TestSaveSettingsAndSpecializedEffectsConverge(t *testing.T) {
	type specialized struct {
		name  string
		run   func(*App) string
		live  func(*effectRecorder) any
		value func(Settings) any
		want  any
	}
	specials := []specialized{
		{name: "opacity", run: func(a *App) string { return a.SetWindowOpacity(83) }, live: func(r *effectRecorder) any { return r.lastOpacity() }, value: func(s Settings) any { return s.WindowOpacity }, want: 83},
		{name: "hotkey", run: func(a *App) string { return a.SetHotkey("ptt", "Ctrl+X") }, live: func(r *effectRecorder) any { return r.lastPTT() }, value: func(s Settings) any { return s.HotkeyPTT }, want: "Ctrl+X"},
		{name: "always_on_top", run: func(a *App) string { a.SetAlwaysOnTop(true); return "" }, live: func(r *effectRecorder) any { return r.lastAOT() }, value: func(s Settings) any { return s.AlwaysOnTop }, want: true},
	}
	for _, special := range specials {
		for _, saveLast := range []bool{false, true} {
			t.Run(special.name+"_saveLast_"+map[bool]string{false: "false", true: "true"}[saveLast], func(t *testing.T) {
				a, recorder := newEffectTestApp(t)
				saved := a.GetSettings()
				saved.WindowOpacity = 71
				saved.HotkeyPTT = "Ctrl+S"
				saved.AlwaysOnTop = false
				runSave := func() {
					if got := a.SaveSettings(saved); got != "" {
						t.Fatal(got)
					}
				}
				if saveLast {
					if got := special.run(a); got != "" {
						t.Fatal(got)
					}
					runSave()
				} else {
					runSave()
					if got := special.run(a); got != "" {
						t.Fatal(got)
					}
				}
				want := special.want
				if saveLast {
					want = special.value(saved)
				}
				if got := special.live(recorder); got != want {
					t.Fatalf("live %s = %v, want %v", special.name, got, want)
				}
				memory, disk := a.GetSettings(), loadSettingsAt(a.settingsPath)
				if got := special.value(memory); got != want {
					t.Fatalf("memory %s = %v, want %v", special.name, got, want)
				}
				if got := special.value(disk); got != want {
					t.Fatalf("disk %s = %v, want %v", special.name, got, want)
				}
			})
		}
	}

	// These overlaps exercise the actual persistence/effect interleaving, not
	// just call order. The old operation is paused after taking its first
	// snapshot and before it can claim an effect-family generation; the newer
	// operation must fully persist and apply before the old one resumes.
	for _, special := range specials {
		t.Run("overlap_save_then_"+special.name, func(t *testing.T) {
			a, recorder := newEffectTestApp(t)
			saved := a.GetSettings()
			saved.WindowOpacity = 71
			saved.HotkeyPTT = "Ctrl+S"
			saved.AlwaysOnTop = false
			entered, release := installEffectBarrier(a)
			oldDone := make(chan string, 1)
			go func() { oldDone <- a.SaveSettings(saved) }()
			<-entered // Save's first (hotkey) family has its older ticket.
			if got := special.run(a); got != "" {
				t.Fatal(got)
			}
			want := saved
			switch special.name {
			case "opacity":
				want.WindowOpacity = special.want.(int)
			case "hotkey":
				want.HotkeyPTT = special.want.(string)
			case "always_on_top":
				want.AlwaysOnTop = special.want.(bool)
			}
			close(release)
			if got := <-oldDone; got != "" {
				t.Fatal(got)
			}
			assertSettingsLiveConverged(t, a, recorder, want)
		})

		t.Run("overlap_"+special.name+"_then_save", func(t *testing.T) {
			a, recorder := newEffectTestApp(t)
			saved := a.GetSettings()
			saved.WindowOpacity = 71
			saved.HotkeyPTT = "Ctrl+S"
			saved.AlwaysOnTop = false
			entered, release := installEffectBarrier(a)
			oldDone := make(chan string, 1)
			go func() { oldDone <- special.run(a) }()
			<-entered // the specialized family owns the older ticket.
			if got := a.SaveSettings(saved); got != "" {
				t.Fatal(got)
			}
			close(release)
			if got := <-oldDone; got != "" {
				t.Fatal(got)
			}
			assertSettingsLiveConverged(t, a, recorder, saved)
		})
	}
}

// installEffectBarrier blocks exactly the first effect callback. The caller
// starts the old operation only after installing it, so entered identifies its
// ticket/family without time-based scheduling assumptions.
func installEffectBarrier(a *App) (<-chan struct{}, chan<- struct{}) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	a.beforeSettingsEffect = func(_ uint64, _ Settings) {
		if once.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	return entered, release
}

func assertSettingsLiveConverged(t *testing.T, a *App, recorder *effectRecorder, want Settings) {
	t.Helper()
	memory, disk := a.GetSettings(), loadSettingsAt(a.settingsPath)
	if memory.WindowOpacity != want.WindowOpacity || disk.WindowOpacity != want.WindowOpacity {
		t.Fatalf("opacity memory/disk = %d/%d, want %d", memory.WindowOpacity, disk.WindowOpacity, want.WindowOpacity)
	}
	if memory.HotkeyPTT != want.HotkeyPTT || disk.HotkeyPTT != want.HotkeyPTT {
		t.Fatalf("ptt memory/disk = %q/%q, want %q", memory.HotkeyPTT, disk.HotkeyPTT, want.HotkeyPTT)
	}
	if memory.AlwaysOnTop != want.AlwaysOnTop || disk.AlwaysOnTop != want.AlwaysOnTop {
		t.Fatalf("always-on-top memory/disk = %t/%t, want %t", memory.AlwaysOnTop, disk.AlwaysOnTop, want.AlwaysOnTop)
	}
	if got := recorder.lastOpacity(); got != want.WindowOpacity {
		t.Fatalf("live opacity = %v, want %d", got, want.WindowOpacity)
	}
	if got := recorder.lastPTT(); got != want.HotkeyPTT {
		t.Fatalf("live ptt = %v, want %q", got, want.HotkeyPTT)
	}
	if got := recorder.lastAOT(); got != want.AlwaysOnTop {
		t.Fatalf("live always-on-top = %v, want %t", got, want.AlwaysOnTop)
	}
}

type effectRecorder struct {
	mu      sync.Mutex
	opacity []int
	ptt     []string
	aot     []bool
}

func newEffectTestApp(t *testing.T) (*App, *effectRecorder) {
	t.Helper()
	a := &App{
		ctx:          context.Background(),
		settings:     DefaultSettings(),
		settingsPath: filepath.Join(t.TempDir(), "settings.json"),
		hotkeys:      map[string]*hotkeyReg{},
		eventEmit:    func(string, any) {},
	}
	r := &effectRecorder{}
	originalOpacity, originalHotkeys, originalAOT := windowOpacityApply, settingsHotkeyApplier, alwaysOnTopApply
	t.Cleanup(func() {
		windowOpacityApply = originalOpacity
		settingsHotkeyApplier = originalHotkeys
		alwaysOnTopApply = originalAOT
	})
	windowOpacityApply = func(value int) error {
		r.mu.Lock()
		r.opacity = append(r.opacity, value)
		r.mu.Unlock()
		return nil
	}
	settingsHotkeyApplier = func(_ *App, action, spec string) {
		if action == "ptt" {
			r.mu.Lock()
			r.ptt = append(r.ptt, spec)
			r.mu.Unlock()
		}
	}
	alwaysOnTopApply = func(_ context.Context, value bool) {
		r.mu.Lock()
		r.aot = append(r.aot, value)
		r.mu.Unlock()
	}
	return a, r
}

func (r *effectRecorder) lastOpacity() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.opacity) == 0 {
		return nil
	}
	return r.opacity[len(r.opacity)-1]
}

func (r *effectRecorder) lastPTT() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ptt) == 0 {
		return nil
	}
	return r.ptt[len(r.ptt)-1]
}

func (r *effectRecorder) lastAOT() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.aot) == 0 {
		return nil
	}
	return r.aot[len(r.aot)-1]
}

func TestSettingsFailedTransactionDoesNotLeakIntoQueuedMutation(t *testing.T) {
	olderErr := errors.New("older mutation failed")
	newerErr := errors.New("newer mutation failed")

	for _, test := range []struct {
		name       string
		newerFails bool
	}{
		{name: "older fails while newer different-family mutation succeeds"},
		{name: "both mutations fail", newerFails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, recorder := newEffectTestApp(t)
			if got := a.SetWindowOpacity(64); got != "" {
				t.Fatalf("seed opacity: %s", got)
			}
			if got := a.SetHotkey("ptt", "Ctrl+Q"); got != "" {
				t.Fatalf("seed hotkey: %s", got)
			}
			before := a.GetSettings()

			oldWriterEntered := make(chan struct{})
			newerQueued := make(chan struct{})
			releaseOldWriter := make(chan struct{})
			var transactionCalls atomic.Int32
			a.beforeSettingsTransaction = func() {
				if transactionCalls.Add(1) == 2 {
					close(newerQueued)
				}
			}

			originalWriter := settingsSnapshotWriter
			var active, peak atomic.Int32
			settingsSnapshotWriter = func(path string, snapshot Settings) error {
				current := active.Add(1)
				for {
					seen := peak.Load()
					if current <= seen || peak.CompareAndSwap(seen, current) {
						break
					}
				}
				defer active.Add(-1)
				switch {
				case snapshot.WindowOpacity == 31:
					close(oldWriterEntered)
					<-releaseOldWriter
					return olderErr
				case snapshot.HotkeyPTT == "Ctrl+B" && test.newerFails:
					return newerErr
				default:
					return originalWriter(path, snapshot)
				}
			}
			t.Cleanup(func() { settingsSnapshotWriter = originalWriter })

			oldDone := make(chan string, 1)
			go func() { oldDone <- a.SetWindowOpacity(31) }()
			<-oldWriterEntered
			newDone := make(chan string, 1)
			go func() { newDone <- a.SetHotkey("ptt", "Ctrl+B") }()
			<-newerQueued // newer has tried to enter while old owns the transaction.

			// The paused older candidate is not yet observable. This is the
			// interleaving that optimistic staging got wrong.
			if got := a.GetSettings(); got.WindowOpacity != before.WindowOpacity || got.HotkeyPTT != before.HotkeyPTT {
				t.Fatalf("uncommitted candidate leaked into memory: got=%+v want=%+v", got, before)
			}
			if got := loadSettingsAt(a.settingsPath); got.WindowOpacity != before.WindowOpacity || got.HotkeyPTT != before.HotkeyPTT {
				t.Fatalf("uncommitted candidate leaked to disk: got=%+v want=%+v", got, before)
			}

			close(releaseOldWriter)
			if got := <-oldDone; got != olderErr.Error() {
				t.Fatalf("older result = %q, want %q", got, olderErr)
			}
			if got := <-newDone; test.newerFails && got != newerErr.Error() {
				t.Fatalf("newer failure = %q, want %q", got, newerErr)
			} else if !test.newerFails && got != "" {
				t.Fatalf("newer success = %q", got)
			}
			if peak.Load() != 1 {
				t.Fatalf("overlapping settings writes peak = %d", peak.Load())
			}

			want := before
			if !test.newerFails {
				want.HotkeyPTT = "Ctrl+B"
			}
			memory, disk := a.GetSettings(), loadSettingsAt(a.settingsPath)
			if memory.WindowOpacity != want.WindowOpacity || disk.WindowOpacity != want.WindowOpacity ||
				memory.HotkeyPTT != want.HotkeyPTT || disk.HotkeyPTT != want.HotkeyPTT {
				t.Fatalf("memory/disk did not converge: memory=%+v disk=%+v want=%+v", memory, disk, want)
			}
			if got := recorder.lastOpacity(); got != want.WindowOpacity {
				t.Fatalf("live opacity = %v, want %d", got, want.WindowOpacity)
			}
			if got := recorder.lastPTT(); got != want.HotkeyPTT {
				t.Fatalf("live ptt = %v, want %q", got, want.HotkeyPTT)
			}
		})
	}
}
