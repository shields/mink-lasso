// Copyright © 2026 Michael Shields
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

package walkui

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/tailscale/walk"
	"github.com/tailscale/walk/declarative"
	"github.com/tailscale/win"

	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/engine"
	"msrl.dev/mink-lasso/internal/ui/model"
	"msrl.dev/mink-lasso/internal/winutil"
)

// Options configures Run. See the package doc for the split of
// responsibility between this package and internal/ui/model.
type Options struct {
	Config config.Config
	Model  *model.Model
	Events <-chan engine.Event
	Logger *slog.Logger
}

// sendFileFilter is the FileDialog filter for File > Send file…, matching
// the extensions internal/config.Default watches for.
const sendFileFilter = "G-code files (*.nc;*.txt;*.cnc;*.tap;*.eia;*.htg;*.wiz;*.gcode;*.ngc)|" +
	"*.nc;*.txt;*.cnc;*.tap;*.eia;*.htg;*.wiz;*.gcode;*.ngc|All files (*.*)|*.*"

// binding holds every widget reference the running window needs to repaint
// itself, plus the small amount of UI-only state (a re-entrancy guard for
// the checkbox, whether the window is really allowed to close) that has no
// home in the portable model.
type binding struct {
	model  *model.Model
	logger *slog.Logger

	minimizeToTray bool

	mw     *walk.MainWindow
	notify *walk.NotifyIcon

	serialEdit  *walk.LineEdit
	statusLabel *walk.Label

	stateLabel *walk.Label
	fileLabel  *walk.Label
	lineLabel  *walk.Label
	jobsLabel  *walk.Label
	gateLabel  *walk.Label
	progress   *walk.ProgressBar

	watchPathEdit *walk.LineEdit
	uploadCheck   *walk.CheckBox
	modeLabel     *walk.Label
	pendingLabel  *walk.Label

	transfersTV  *walk.TableView
	transfers    *transfersModel
	toolsTV      *walk.TableView
	tools        *toolsModel
	retryAction  *walk.Action
	showInFolder *walk.Action

	logEdit *walk.TextEdit

	painting bool // true while a checkbox repaint must not re-trigger its own handler
	exiting  bool // true once a real close has been requested (bypasses minimize-to-tray)

	exited atomic.Bool // true once the message loop has returned; guards stray Synchronize calls
}

// Run builds the main window and tray icon, wires them to opts.Model, and
// runs the message loop on the calling goroutine until the user exits or ctx
// is done. It must be called from the main goroutine, and only once per
// process: walk.InitApp may only be called once.
func Run(ctx context.Context, opts Options) error {
	app, err := walk.InitApp()
	if err != nil {
		return err
	}

	b := &binding{
		model:          opts.Model,
		logger:         opts.Logger,
		minimizeToTray: opts.Config.MinimizeToTray,
		transfers:      &transfersModel{},
		tools:          &toolsModel{},
	}

	if err := b.build(opts); err != nil {
		return err
	}
	// walk's own WM_CLOSE handling calls Application.Exit unconditionally
	// unless this is disabled, ignoring whatever onClosing did with
	// *canceled — so minimize-to-tray would otherwise always fall through
	// to a full exit. The real-exit paths (requestExit, ctx.Done below, and
	// onClosing's own fallthrough when minimize-to-tray is off) call Exit
	// themselves.
	b.mw.SetExitOnClose(false)
	defer b.disposeTray()

	if err := b.buildTray(opts); err != nil {
		return err
	}

	b.mw.Closing().Attach(b.onClosing)

	opts.Model.SetOnChange(func(ch model.Changes) {
		if b.exited.Load() {
			return
		}

		app.Synchronize(func() { b.paint(ch) })
	})

	go b.pumpEvents(app, opts.Events)

	go func() {
		<-ctx.Done()

		if b.exited.Load() {
			return
		}

		app.Synchronize(func() {
			b.exiting = true

			if err := b.mw.Close(); err != nil {
				b.logger.Warn("close main window", "error", err)
			}

			app.Exit(0)
		})
	}()

	app.Run()
	b.exited.Store(true)

	return nil
}

// pumpEvents applies every engine event to the model and repaints, on the UI
// goroutine, for as long as events is open. It keeps ranging even after the
// message loop has returned so the caller (which must keep draining
// Engine.Events until it closes) never blocks on us.
func (b *binding) pumpEvents(app *walk.Application, events <-chan engine.Event) {
	for ev := range events {
		if b.exited.Load() {
			continue
		}

		app.Synchronize(func() {
			ch := b.model.Apply(ev)
			b.paint(ch)
		})
	}
}

// onClosing implements the Closing contract: minimize to tray unless the
// close was requested through requestExit (the Exit menu item, the tray
// Exit item, or ctx being done), or minimize-to-tray is off, in which case
// the window closing for any reason (including the user's own X button) is
// a real exit — SetExitOnClose(false) above means walk won't provide that
// exit on its own.
func (b *binding) onClosing(canceled *bool, _ walk.CloseReason) {
	if b.exiting {
		return
	}

	if b.minimizeToTray {
		*canceled = true
		b.mw.Hide()

		return
	}

	b.exiting = true
	walk.App().Exit(0)
}

// requestExit lets the window really close, ending the message loop. It
// must run on the UI goroutine. Closing the window no longer ends the
// message loop by itself (see SetExitOnClose in Run), so this calls Exit
// explicitly.
func (b *binding) requestExit() {
	b.exiting = true

	if err := b.mw.Close(); err != nil {
		b.logger.Warn("close main window", "error", err)
	}

	walk.App().Exit(0)
}

// showAndActivate un-minimizes the window from the tray. Neither Show nor
// Activate restores a window the user minimized with the native title-bar
// button (Show is a no-op once WS_VISIBLE is already set, and
// SetActiveWindow does not change the show state), so an iconic window is
// restored explicitly first.
func (b *binding) showAndActivate() {
	if win.IsIconic(b.mw.Handle()) {
		win.ShowWindow(b.mw.Handle(), win.SW_RESTORE)
	}

	b.mw.Show()

	if err := b.mw.Activate(); err != nil {
		b.logger.Warn("activate main window", "error", err)
	}
}

func (b *binding) showError(title string, err error) {
	// walk.MsgBox is deprecated upstream in favor of TaskDialog, but it is
	// the simplest correct modal for a one-line error and matches the rest
	// of this binding's minimal footprint.
	walk.MsgBox(b.mw, title, err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError) //nolint:staticcheck // see comment above
}

func (b *binding) build(opts Options) error {
	watch := opts.Model.Watch()

	// declarative's Create applies the Checked property after
	// OnCheckedChanged is already attached, so a persisted true value
	// would otherwise fire the handler during construction — before the
	// window is shown or the user has touched anything. Guard it the same
	// way paintWatch guards its own programmatic updates.
	b.painting = true
	defer func() { b.painting = false }()

	return declarative.MainWindow{
		AssignTo: &b.mw,
		Title:    opts.Model.Title(),
		MinSize:  declarative.Size{Width: 760, Height: 560},
		Visible:  !opts.Config.StartMinimized,
		Layout:   declarative.VBox{},
		MenuItems: []declarative.MenuItem{
			declarative.Menu{
				Text: "&File",
				Items: []declarative.MenuItem{
					declarative.Action{Text: "&Send file…", OnTriggered: b.onSendFile},
					declarative.Separator{},
					declarative.Action{Text: "E&xit", OnTriggered: b.requestExit},
				},
			},
			declarative.Menu{
				Text: "&Help",
				Items: []declarative.MenuItem{
					declarative.Action{Text: "&About", OnTriggered: b.onAbout},
				},
			},
		},
		Children: []declarative.Widget{
			b.controllerGroup(opts),
			b.machineGroup(opts),
			b.watchGroup(watch),
			b.tabs(),
			declarative.TextEdit{
				AssignTo: &b.logEdit,
				ReadOnly: true,
				VScroll:  true,
				Text:     strings.Join(opts.Model.LogLines(), "\r\n"),
			},
		},
	}.Create()
}

func (b *binding) controllerGroup(opts Options) declarative.GroupBox {
	return declarative.GroupBox{
		Title:  "Controller",
		Layout: declarative.HBox{},
		Children: []declarative.Widget{
			declarative.LineEdit{
				AssignTo:  &b.serialEdit,
				CueBanner: "G3-12345",
				Text:      opts.Model.SerialText(),
			},
			declarative.PushButton{Text: "Apply", OnClicked: b.onApplySerial},
			declarative.Label{AssignTo: &b.statusLabel, Text: opts.Model.StatusLine()},
			declarative.HSpacer{},
		},
	}
}

func (b *binding) machineGroup(opts Options) declarative.GroupBox {
	mp := opts.Model.Machine()

	return declarative.GroupBox{
		Title:  "Machine",
		Layout: declarative.VBox{},
		Children: []declarative.Widget{
			declarative.Composite{
				Layout: declarative.HBox{},
				Children: []declarative.Widget{
					declarative.Label{AssignTo: &b.stateLabel, Text: mp.StateText},
					declarative.Label{AssignTo: &b.fileLabel, Text: mp.File},
					declarative.Label{AssignTo: &b.lineLabel, Text: mp.LineText},
					declarative.HSpacer{},
				},
			},
			declarative.Composite{
				Layout: declarative.HBox{},
				Children: []declarative.Widget{
					declarative.Label{AssignTo: &b.jobsLabel, Text: mp.JobsText},
					declarative.Label{AssignTo: &b.gateLabel, Text: mp.GateText},
					declarative.HSpacer{},
				},
			},
			declarative.ProgressBar{AssignTo: &b.progress, MinValue: 0, MaxValue: 100, Value: mp.Progress},
		},
	}
}

func (b *binding) watchGroup(watch model.WatchPanel) declarative.GroupBox {
	return declarative.GroupBox{
		Title:  "Watch folder",
		Layout: declarative.VBox{},
		Children: []declarative.Widget{
			declarative.Composite{
				Layout: declarative.HBox{},
				Children: []declarative.Widget{
					declarative.LineEdit{AssignTo: &b.watchPathEdit, ReadOnly: true, Text: watch.Dir},
					declarative.PushButton{Text: "Browse…", OnClicked: b.onBrowse},
				},
			},
			declarative.CheckBox{
				AssignTo:         &b.uploadCheck,
				Text:             "Upload while machining",
				Checked:          watch.UploadWhileMachining,
				OnCheckedChanged: b.onUploadWhileMachiningChanged,
			},
			declarative.Composite{
				Layout: declarative.HBox{},
				Children: []declarative.Widget{
					declarative.PushButton{Text: "Open folder", OnClicked: b.onOpenFolder},
					declarative.PushButton{Text: "Open sent folder", OnClicked: b.onOpenSentFolder},
					declarative.HSpacer{},
				},
			},
			declarative.Label{AssignTo: &b.modeLabel, Text: watch.ModeText},
			declarative.Label{AssignTo: &b.pendingLabel, Text: watch.PendingText},
		},
	}
}

func (b *binding) tabs() declarative.TabWidget {
	retry := declarative.Action{AssignTo: &b.retryAction, Text: "Retry", OnTriggered: b.onRetry}
	showFolder := declarative.Action{AssignTo: &b.showInFolder, Text: "Show in folder", OnTriggered: b.onShowInFolder}

	return declarative.TabWidget{
		Pages: []declarative.TabPage{
			{
				Title:  "Transfers",
				Layout: declarative.VBox{},
				Children: []declarative.Widget{
					declarative.TableView{
						AssignTo: &b.transfersTV,
						Model:    b.transfers,
						Columns: []declarative.TableViewColumn{
							{Title: "Name", Width: 220},
							{Title: "Size", Width: 80},
							{Title: "State", Width: 90},
							{Title: "Message", Width: 200},
							{Title: "Time", Width: 130},
						},
						ContextMenuItems:      []declarative.MenuItem{retry, showFolder},
						OnCurrentIndexChanged: b.updateRetryEnabled,
					},
				},
			},
			{
				Title:  "Tools",
				Layout: declarative.VBox{},
				Children: []declarative.Widget{
					declarative.TableView{
						AssignTo: &b.toolsTV,
						Model:    b.tools,
						Columns: []declarative.TableViewColumn{
							{Title: "Index", Width: 60},
							{Title: "Name", Width: 260},
						},
					},
					declarative.PushButton{Text: "Refresh", OnClicked: b.model.RefreshTools},
				},
			},
		},
	}
}

func (b *binding) onApplySerial() {
	if err := b.model.ApplySerial(b.serialEdit.Text()); err != nil {
		b.showError("Serial number", err)
	}
}

func (b *binding) onSendFile() {
	dlg := &walk.FileDialog{Title: "Send file", Filter: sendFileFilter}

	ok, err := dlg.ShowOpen(b.mw)
	if err != nil {
		b.showError("Send file", err)

		return
	}

	if !ok {
		return
	}

	if err := b.model.SendFile(dlg.FilePath); err != nil {
		b.showError("Send file", err)
	}
}

func (b *binding) onAbout() {
	style := walk.MsgBoxOK | walk.MsgBoxIconInformation
	walk.MsgBox(b.mw, "About", b.model.AboutText(), style) //nolint:staticcheck // see showError
}

func (b *binding) onBrowse() {
	// InitialDirPath is deliberately omitted: ShowBrowseFolder (unlike
	// ShowOpen) resolves it into BROWSEINFO.PidlRoot, which restricts the
	// whole tree to that folder and its subfolders rather than merely
	// seeding a starting selection, permanently walling off the picker
	// once a watch folder has ever been chosen.
	dlg := &walk.FileDialog{Title: "Choose the folder to watch"}

	ok, err := dlg.ShowBrowseFolder(b.mw)
	if err != nil {
		b.showError("Watch folder", err)

		return
	}

	if !ok {
		return
	}

	if err := b.model.SetWatchDir(dlg.FilePath); err != nil {
		b.showError("Watch folder", err)
	}
}

func (b *binding) onUploadWhileMachiningChanged() {
	if b.painting {
		return
	}

	checked := b.uploadCheck.Checked()
	if err := b.model.SetUploadWhileMachining(checked); err != nil {
		b.painting = true
		b.uploadCheck.SetChecked(!checked)
		b.painting = false

		b.showError("Upload while machining", err)
	}
}

func (b *binding) onOpenFolder() {
	b.openFolder("Open folder", b.model.FolderPath())
}

func (b *binding) onOpenSentFolder() {
	b.openFolder("Open sent folder", b.model.SentFolderPath())
}

// openFolder opens path in Explorer, reporting a failure the same way every
// other user-triggered action in this file does.
func (b *binding) openFolder(title, path string) {
	if path == "" {
		return
	}

	if err := winutil.OpenFolder(path); err != nil {
		b.showError(title, err)
	}
}

// selectedTransfer returns the transfer row currently selected in the
// Transfers table, if any.
func (b *binding) selectedTransfer() (model.TransferRow, bool) {
	idx := b.transfersTV.CurrentIndex()
	if idx < 0 || idx >= len(b.transfers.rows) {
		return model.TransferRow{}, false
	}

	return b.transfers.rows[idx], true
}

func (b *binding) updateRetryEnabled() {
	if b.retryAction == nil {
		return
	}

	enabled := false
	if row, ok := b.selectedTransfer(); ok {
		enabled = b.model.RetryEnabled(row.Name)
	}

	if err := b.retryAction.SetEnabled(enabled); err != nil {
		b.logger.Warn("set retry action enabled", "error", err)
	}
}

func (b *binding) onRetry() {
	if row, ok := b.selectedTransfer(); ok && b.model.RetryEnabled(row.Name) {
		b.model.Retry(row.Name)
	}
}

func (b *binding) onShowInFolder() {
	if row, ok := b.selectedTransfer(); ok {
		b.openFolder("Show in folder", row.Path)
	}
}
