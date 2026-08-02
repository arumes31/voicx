// tray.go implements the system tray integration (287-290) via
// getlantern/systray — the one new client dependency this wave, justified
// because Wails v2 has no tray API.
//
// Threading: systray.Run must own the calling goroutine's message loop; on
// macOS that must be the process main thread, on Windows any single
// dedicated goroutine works. main() therefore runs systray on the main
// thread and wails.Run in a goroutine — the order that is safe on both.
package main

import (
	"bytes"
	"encoding/binary"
	"log"
	"sync"

	"github.com/getlantern/systray"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// tray is the shared tray controller (nil until initTray runs).
var trayCtl *tray

// tray wraps the systray menu and badge state.
type tray struct {
	app *App

	mu       sync.Mutex
	mentions int
	ptt      bool
	muted    bool
	visible  bool

	miShowHide *systray.MenuItem
	miMute     *systray.MenuItem
	miDeafen   *systray.MenuItem
}

// initTray starts the tray on the calling goroutine (blocks until quit).
// onReady wires the menu after the app started.
func initTray(a *App) {
	trayCtl = &tray{app: a, visible: true}
	systray.Run(trayCtl.onReady, func() { log.Printf("tray exit") })
}

// onReady builds the tray menu (called on the systray thread).
func (t *tray) onReady() {
	systray.SetIcon(trayIcon())
	t.updateTitle()
	t.miShowHide = systray.AddMenuItem("Hide voicx", "show/hide the window")
	systray.AddSeparator()
	t.miMute = systray.AddMenuItem("Mute", "toggle microphone mute")
	t.miDeafen = systray.AddMenuItem("Deafen", "toggle deafen")
	systray.AddSeparator()
	miDisconnect := systray.AddMenuItem("Disconnect", "disconnect the active server tab")
	miQuit := systray.AddMenuItem("Quit", "quit voicx")

	// recover is per-goroutine: the menu event loop needs its own guard (331).
	go guardCrash("tray", func() {
		for {
			select {
			case <-t.miShowHide.ClickedCh:
				t.toggleWindow()
			case <-t.miMute.ClickedCh:
				t.app.emitHotkey("mute_toggle")
			case <-t.miDeafen.ClickedCh:
				t.app.emitHotkey("deafen_toggle")
			case <-miDisconnect.ClickedCh:
				t.app.Disconnect()
			case <-miQuit.ClickedCh:
				systray.Quit()
				if t.app.ctx != nil {
					wailsRuntime.Quit(t.app.ctx)
				}
			}
		}
	})
}

// toggleWindow shows or hides the main window (287).
func (t *tray) toggleWindow() {
	t.mu.Lock()
	t.visible = !t.visible
	visible := t.visible
	t.mu.Unlock()
	if t.app.ctx == nil {
		return
	}
	if visible {
		wailsRuntime.WindowShow(t.app.ctx)
		t.miShowHide.SetTitle("Hide voicx")
	} else {
		wailsRuntime.WindowHide(t.app.ctx)
		t.miShowHide.SetTitle("Show voicx")
	}
}

// trayMarkHidden records that the window was hidden by something other than
// the tray menu (288 minimize-to-tray), so the next menu click shows it again
// instead of hiding an already-hidden window.
func trayMarkHidden() {
	if trayCtl == nil {
		return
	}
	trayCtl.mu.Lock()
	trayCtl.visible = false
	trayCtl.mu.Unlock()
	if trayCtl.miShowHide != nil {
		trayCtl.miShowHide.SetTitle("Show voicx")
	}
}

// updateTitle refreshes the tray title/tooltip (289 PTT state, 290 badge).
func (t *tray) updateTitle() {
	t.mu.Lock()
	mentions, ptt, muted := t.mentions, t.ptt, t.muted
	t.mu.Unlock()
	title := "voicx"
	if ptt {
		title += " ● TALKING"
	} else if muted {
		title += " (muted)"
	}
	tooltip := "voicx voice client"
	if mentions > 0 {
		tooltip += " — " + itoa(mentions) + " unread mention(s)"
	}
	systray.SetTitle(title)
	systray.SetTooltip(tooltip)
}

// itoa is a tiny int->string helper (avoids strconv for one call site).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// traySetPTT reflects the PTT state in the tray title (289).
func traySetPTT(active bool) {
	if trayCtl == nil {
		return
	}
	trayCtl.mu.Lock()
	trayCtl.ptt = active
	trayCtl.mu.Unlock()
	trayCtl.updateTitle()
}

// traySetMuted reflects the mute state in the tray title.
func traySetMuted(muted bool) {
	if trayCtl == nil {
		return
	}
	trayCtl.mu.Lock()
	trayCtl.muted = muted
	trayCtl.mu.Unlock()
	trayCtl.updateTitle()
}

// trayAddMention bumps the mention badge (290) and refreshes the tooltip.
func trayAddMention() {
	if trayCtl == nil {
		return
	}
	trayCtl.mu.Lock()
	trayCtl.mentions++
	trayCtl.mu.Unlock()
	trayCtl.updateTitle()
}

// trayClearMentions resets the badge (window focused / tab switched).
func trayClearMentions() {
	if trayCtl == nil {
		return
	}
	trayCtl.mu.Lock()
	trayCtl.mentions = 0
	trayCtl.mu.Unlock()
	trayCtl.updateTitle()
}

// TrayMention bumps the tray badge for a mention that arrived in the ACTIVE
// tab (290). Background tabs are counted by the event relay, which never sees
// the active tab's frames, so the frontend has to report those itself.
func (a *App) TrayMention() {
	trayAddMention()
}

// TrayClearMentions resets the tray badge (290), e.g. once the window is
// focused again and the user has necessarily seen the messages.
func (a *App) TrayClearMentions() {
	trayClearMentions()
}

// trayIcon builds a minimal 16x16 32bpp ICO at runtime (signal-green square
// with a transparent border) so no asset file is needed.
func trayIcon() []byte {
	const size = 16
	// BITMAPINFOHEADER with biHeight = 2*size (XOR + AND masks).
	var buf bytes.Buffer
	// ICONDIR.
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // count
	// ICONDIRENTRY.
	buf.WriteByte(size)                                     // width
	buf.WriteByte(size)                                     // height
	buf.WriteByte(0)                                        // colors (0 = >8bpp)
	buf.WriteByte(0)                                        // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // bpp
	imageSize := uint32(40 + size*size*4 + size*size/8)
	_ = binary.Write(&buf, binary.LittleEndian, imageSize)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(6+16)) // data offset
	// BITMAPINFOHEADER.
	_ = binary.Write(&buf, binary.LittleEndian, uint32(40))    // header size
	_ = binary.Write(&buf, binary.LittleEndian, int32(size))   // width
	_ = binary.Write(&buf, binary.LittleEndian, int32(size*2)) // height (x2)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))     // planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))    // bpp
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))     // BI_RGB
	_ = binary.Write(&buf, binary.LittleEndian, uint32(size*size*4))
	_ = binary.Write(&buf, binary.LittleEndian, int32(0))
	_ = binary.Write(&buf, binary.LittleEndian, int32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
	// Pixels, bottom-up BGRA: 2px transparent border, green core.
	for y := size - 1; y >= 0; y-- {
		for x := 0; x < size; x++ {
			edge := x < 2 || y < 2 || x >= size-2 || y >= size-2
			if edge {
				buf.Write([]byte{0, 0, 0, 0})
			} else {
				buf.Write([]byte{0xa8, 0xe6, 0x2e, 0xff}) // #2ee6a8 as BGRA
			}
		}
	}
	// AND mask (1bpp, rows padded to 32 bits): all zero (opaque where alpha=ff).
	buf.Write(make([]byte, size*4))
	return buf.Bytes()
}
