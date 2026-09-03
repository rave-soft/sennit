package model

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/editor"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/clipboard"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/richpaste"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/completions"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// If pasted text has more than 10 newlines, treat it as a file attachment.
const pasteLinesThreshold = 10

// If pasted text has more than 1000 columns, treat it as a file attachment.
const pasteColsThreshold = 1000

// handleTextareaHeightChange checks whether the textarea height changed and,
// if so, recalculates the layout. When the chat is in follow mode it keeps
// the view scrolled to the bottom. The returned command, if non-nil, must be
// batched by the caller.
func (m *UI) handleTextareaHeightChange(prevHeight int) tea.Cmd {
	if m.editor.textarea.Height() == prevHeight {
		return nil
	}
	m.updateLayoutAndSize()
	if m.state == uiChat && m.chat.Follow() {
		return m.chat.ScrollToBottomAndAnimate()
	}
	return nil
}

// updateTextarea updates the textarea for msg and then reconciles layout if
// the textarea height changed as a result.
func (m *UI) updateTextarea(msg tea.Msg) tea.Cmd {
	return m.updateTextareaWithPrevHeight(msg, m.editor.textarea.Height())
}

// updateTextareaWithPrevHeight is for cases when the height of the layout may
// have changed.
//
// Particularly, it's for cases where the textarea changes before
// textarea.Update is called (for example, SetValue, Reset, and InsertRune). We
// pass the height from before those changes took place so we can compare
// "before" vs "after" sizing and recalculate the layout if the textarea grew
// or shrank.
func (m *UI) updateTextareaWithPrevHeight(msg tea.Msg, prevHeight int) tea.Cmd {
	ta, cmd := m.editor.textarea.Update(msg)
	m.editor.textarea = ta
	return tea.Batch(cmd, m.handleTextareaHeightChange(prevHeight))
}

// openEditorReadyMsg carries the prepared $EDITOR invocation once
// openEditor's off-loop closure has written the scratch file. Update
// launches the process (see execEditorCmd) when this arrives.
// uiOwned: dispatched by openEditor. Routed by active screen instead, the
// scratch-file prep for one UI's editor could hand its exec launch to
// execEditorCmd on the wrong screen (or the dashboard, which drops it),
// leaving the user's $EDITOR session unstarted.
type openEditorReadyMsg struct {
	uiOwned

	cmd     *exec.Cmd
	tmpPath string
}

// openEditor snapshots the cursor position it needs, then does the actual
// scratch-file IO in the returned tea.Cmd's closure: os.CreateTemp and the
// write both touch disk and must not run on the Update goroutine that a key
// handler calls this from.
func (e *editorState) openEditor(value string, owner *UI) tea.Cmd {
	line := e.textarea.Line() + 1
	col := e.textarea.Column() + 1
	return func() tea.Msg {
		tmpfile, err := os.CreateTemp("", "msg_*.md")
		if err != nil {
			return util.NewErrorMsg(err)
		}
		tmpPath := tmpfile.Name()
		cleanup := func() {
			_ = tmpfile.Close()
			_ = os.Remove(tmpPath)
		}
		if _, err := tmpfile.WriteString(value); err != nil {
			cleanup()
			return util.NewErrorMsg(err)
		}
		if err := tmpfile.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return util.NewErrorMsg(err)
		}
		cmd, err := editor.Command(
			brand.Slug,
			tmpPath,
			editor.AtPosition(line, col),
		)
		if err != nil {
			_ = os.Remove(tmpPath)
			return util.NewErrorMsg(err)
		}
		return openEditorReadyMsg{uiOwned: uiOwned{owner: owner}, cmd: cmd, tmpPath: tmpPath}
	}
}

// execEditorCmd launches the prepared $EDITOR process. Kept separate from
// openEditor's closure so the tea.ExecProcess call — which takes over the
// terminal — happens from Update once the scratch file is ready, not from
// the cmd goroutine that prepared it.
func execEditorCmd(msg openEditorReadyMsg, owner *UI) tea.Cmd {
	return tea.ExecProcess(msg.cmd, func(err error) tea.Msg {
		defer func() {
			_ = os.Remove(msg.tmpPath)
		}()

		if err != nil {
			return util.NewErrorMsg(err)
		}
		content, err := os.ReadFile(msg.tmpPath)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		if len(content) == 0 {
			return util.NewWarnMsg("Message is empty")
		}
		return openEditorMsg{
			uiOwned: uiOwned{owner: owner},
			Text:    strings.TrimSpace(string(content)),
		}
	})
}

// setEditorPrompt configures the textarea prompt function based on whether
// yolo mode or bang mode is enabled.
func (m *UI) setEditorPrompt(yolo bool) {
	if m.editor.bang.isActive() {
		m.editor.textarea.SetPromptFunc(4, func(info textarea.PromptInfo) string { return bangPromptFunc(m.com, info) })
		return
	}
	if yolo {
		m.editor.textarea.SetPromptFunc(4, func(info textarea.PromptInfo) string { return yoloPromptFunc(m.com, info) })
		return
	}
	m.editor.textarea.SetPromptFunc(2, normalPromptFunc)
}

// normalPromptFunc keeps the prompt width as whitespace so multiline text
// stays aligned without visible prompt markers.
func normalPromptFunc(textarea.PromptInfo) string {
	return "  "
}

// yoloPromptFunc returns the yolo mode editor prompt style with warning icon
// and colored dots.
func yoloPromptFunc(com *common.Common, info textarea.PromptInfo) string {
	t := com.Styles
	if info.LineNumber == 0 {
		if info.Focused {
			return t.Editor.PromptYoloIconFocused.Render()
		} else {
			return t.Editor.PromptYoloIconBlurred.Render()
		}
	}
	return "    "
}

// bangPromptFunc returns the bang mode editor prompt style with Turtle-colored
// icon and dots.
func bangPromptFunc(com *common.Common, info textarea.PromptInfo) string {
	t := com.Styles
	if info.LineNumber == 0 {
		if info.Focused {
			return t.Editor.PromptBangIconFocused.Render()
		}
		return t.Editor.PromptBangIconBlurred.Render()
	}
	return "    "
}

// fileCompletionMsg carries the result of insertFileCompletion's off-loop
// file read. fileCmd's closure runs on a different goroutine than Update, so
// it cannot append to m.sess.fileReads directly; Update does that when it
// handles this message.
//
// uiOwned: dispatched by insertFileCompletion. Routed by active screen
// instead, an @-completion picked in a thread's own editor could append
// its attachment to the main screen's editor instead, or vice versa.
type fileCompletionMsg struct {
	uiOwned

	absPath    string
	attachment message.Attachment
}

// insertFileCompletion inserts the selected file path into the textarea,
// replacing the @query, and adds the file as an attachment.
func (m *UI) insertFileCompletion(path string) tea.Cmd {
	prevHeight := m.editor.textarea.Height()
	if !m.editor.completions.replace(&m.editor.textarea, path) {
		return nil
	}
	heightCmd := m.handleTextareaHeightChange(prevHeight)

	// Snapshot session state up front: fileCmd's closure runs on the cmd
	// goroutine and must not read or write m.sess off the Update loop.
	hasSession := m.sess.hasSession()
	var sessionID string
	if hasSession {
		sessionID = m.sess.current.ID
	}
	fileReads := append([]string(nil), m.sess.fileReads...)
	ws := m.com.Workspace
	ctx := m.com.Context()

	fileCmd := func() tea.Msg {
		absPath, _ := filepath.Abs(path)

		if hasSession {
			// Skip attachment if file was already read and hasn't been modified.
			lastRead := ws.FileTrackerLastReadTime(ctx, sessionID, absPath)
			if !lastRead.IsZero() {
				if info, err := os.Stat(path); err == nil && !info.ModTime().After(lastRead) {
					return nil
				}
			}
		} else if slices.Contains(fileReads, absPath) {
			return nil
		}

		// Stat before reading: the @ completion list comes from
		// fsext.ListDirectory, which enumerates every file regardless of
		// size or type, so an oversized pick (a .sqlite, a .pack, a video)
		// must be caught before it's read whole into memory.
		info, err := os.Stat(path)
		if err != nil {
			// If it fails, let the LLM handle it later.
			return nil
		}
		if info.Size() > common.MaxAttachmentSize {
			// The @query text was already inserted as the path by
			// completions.replace above; only the attachment is skipped.
			return util.NewWarnMsg("File is too big to attach (>5mb); inserted path only")
		}

		// Add file as attachment.
		content, err := os.ReadFile(path)
		if err != nil {
			// If it fails, let the LLM handle it later.
			return nil
		}

		mimeType := mimeOf(content)
		if !strings.HasPrefix(mimeType, "text/") && !strings.HasPrefix(mimeType, "image/") {
			// Anything that isn't text or an image (a .sqlite, a .pack, an
			// .mp4) would otherwise ride along as octet-stream forever in
			// the session history. Leave just the path in the input; the
			// LLM can read it with a tool if it actually needs the bytes.
			// Say so rather than dropping the attachment silently: the
			// oversized branch above warns, and picking a file only to
			// find nothing attached and no reason given is worse than
			// either outcome.
			return util.NewWarnMsg("Attached files must be text or images; inserted path only")
		}

		return fileCompletionMsg{
			uiOwned: uiOwned{owner: m},
			absPath: absPath,
			attachment: message.Attachment{
				FilePath: path,
				FileName: filepath.Base(path),
				MimeType: mimeType,
				Content:  content,
			},
		}
	}
	return tea.Batch(heightCmd, fileCmd)
}

// insertMCPResourceCompletion inserts the selected resource into the textarea,
// replacing the @query, and adds the resource as an attachment.
func (m *UI) insertMCPResourceCompletion(item completions.ResourceCompletionValue) tea.Cmd {
	displayText := cmp.Or(item.Title, item.URI)

	prevHeight := m.editor.textarea.Height()
	if !m.editor.completions.replace(&m.editor.textarea, displayText) {
		return nil
	}
	heightCmd := m.handleTextareaHeightChange(prevHeight)

	ws := m.com.Workspace
	ctx := m.com.Context()
	resourceCmd := func() tea.Msg {
		contents, err := ws.ReadMCPResource(
			ctx,
			item.MCPName,
			item.URI,
		)
		if err != nil {
			slog.Warn("Failed to read MCP resource", "uri", item.URI, "error", err)
			return nil
		}
		if len(contents) == 0 {
			return nil
		}

		content := contents[0]
		var data []byte
		if content.Text != "" {
			data = []byte(content.Text)
		} else if len(content.Blob) > 0 {
			data = content.Blob
		}
		if len(data) == 0 {
			return nil
		}

		mimeType := item.MIMEType
		if mimeType == "" && content.MIMEType != "" {
			mimeType = content.MIMEType
		}
		if mimeType == "" {
			mimeType = "text/plain"
		}

		return message.Attachment{
			FilePath: item.URI,
			FileName: displayText,
			MimeType: mimeType,
			Content:  data,
		}
	}
	return tea.Batch(heightCmd, resourceCmd)
}

func mimeOf(content []byte) string {
	mimeBufferSize := min(512, len(content))
	return http.DetectContentType(content[:mimeBufferSize])
}

// checkBangModeAfterPaste engages bang mode when pasted text starts with
// optional whitespace followed by "!". It strips the prefix and adjusts
// the cursor, mirroring the keypress bang-mode entry logic.
func (m *UI) checkBangModeAfterPaste() {
	if m.editor.bang.enterFromLeadingPrefix(&m.editor.textarea, "", m.editor.textarea.Column()) {
		m.setEditorPrompt(m.wsCache.yoloModeCached())
	}
}

func (m *UI) handlePasteMsg(msg tea.PasteMsg) tea.Cmd {
	// Normalize \r\n before the textarea sanitizer sees it.
	msg.Content = strings.ReplaceAll(msg.Content, "\r\n", "\n")

	if m.dialog.HasDialogs() {
		return m.handleDialogMsg(msg)
	}

	if m.focus != uiFocusEditor {
		return nil
	}

	if hasPasteExceededThreshold(msg) {
		// Snapshot pasteIdx up front: the closure below runs on the cmd
		// goroutine and must not read m.editor off the Update loop.
		pasteIdx := m.pasteIdx()
		return func() tea.Msg {
			content := []byte(msg.Content)
			if int64(len(content)) > common.MaxAttachmentSize {
				// A tea.Cmd here would be delivered as a message and
				// dropped: this command's return value is the message.
				return util.NewWarnMsg("Paste is too big (>5mb)")
			}
			name := fmt.Sprintf("paste_%d.txt", pasteIdx)
			return common.AttachmentFromBytes(name, name, content)
		}
	}

	// Attempt to parse pasted content as file paths. Whether they all exist
	// and are valid images decides the branch below, but checking that
	// means stat'ing the filesystem — disk IO that must not run on the
	// Update goroutine. When there are no candidate paths, no stat is
	// needed and the usual text paste happens inline, same as always; only
	// a non-empty candidate list detours through pasteFilesCheckedMsg
	// (handled in Update) to do the stat off-thread.
	paths := fsext.ParsePastedFiles(msg.Content)
	prevHeight := m.editor.textarea.Height()
	if len(paths) == 0 {
		cmd := m.updateTextareaWithPrevHeight(msg, prevHeight)
		m.checkBangModeAfterPaste()
		return cmd
	}
	return func() tea.Msg {
		return pasteFilesCheckedMsg{
			uiOwned:    uiOwned{owner: m},
			msg:        msg,
			paths:      paths,
			valid:      pastedFilesExistAndValid(paths),
			prevHeight: prevHeight,
		}
	}
}

// pastedFilesExistAndValid reports whether paths is non-empty and every
// entry exists on disk and has an allowed image extension. Runs off the
// Update goroutine (os.Stat is disk IO) — see handlePasteMsg.
func pastedFilesExistAndValid(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, path := range paths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return false
		}

		lowerPath := strings.ToLower(path)
		isValid := false
		for _, ext := range common.AllowedImageTypes {
			if strings.HasSuffix(lowerPath, ext) {
				isValid = true
				break
			}
		}
		if !isValid {
			return false
		}
	}
	return true
}

// pasteFilesCheckedMsg carries the result of handlePasteMsg's off-loop
// filesystem check for a pasted-file-paths paste. Update applies it via
// applyPasteFilesChecked.
//
// uiOwned: dispatched by handlePasteMsg. Routed by active screen instead,
// a paste into a thread's own editor could apply its file-attachment
// result to the main screen's textarea, or vice versa.
type pasteFilesCheckedMsg struct {
	uiOwned

	msg        tea.PasteMsg
	paths      []string
	valid      bool
	prevHeight int
}

// applyPasteFilesChecked finishes the paste handling that pastedFilesExistAndValid's
// result decides between: pasting the content as text if the paths were not
// all valid images, or attaching each path as a file otherwise.
func (m *UI) applyPasteFilesChecked(msg pasteFilesCheckedMsg) tea.Cmd {
	if !msg.valid {
		cmd := m.updateTextareaWithPrevHeight(msg.msg, msg.prevHeight)
		m.checkBangModeAfterPaste()
		return cmd
	}

	var cmds []tea.Cmd
	for _, path := range msg.paths {
		cmds = append(cmds, handleFilePathPaste(path))
	}
	return tea.Batch(cmds...)
}

func hasPasteExceededThreshold(msg tea.PasteMsg) bool {
	var (
		lineCount = 0
		colCount  = 0
	)
	for line := range strings.SplitSeq(msg.Content, "\n") {
		lineCount++
		colCount = max(colCount, len(line))

		if lineCount > pasteLinesThreshold || colCount > pasteColsThreshold {
			return true
		}
	}
	return false
}

func handleFilePathPaste(path string) tea.Cmd {
	return func() tea.Msg {
		fileInfo, err := os.Stat(path)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		if fileInfo.IsDir() {
			return util.NewWarnMsg("Cannot attach a directory")
		}
		if fileInfo.Size() > common.MaxAttachmentSize {
			return util.NewWarnMsg("File is too big (>5mb)")
		}

		attachment, err := common.AttachmentFromPath(path)
		if err != nil {
			return util.NewErrorMsg(err)
		}

		return attachment
	}
}

// richPasteTimeout bounds the whole rich-paste round trip, downloads
// included, so that a slow host cannot wedge the paste keybinding.
const richPasteTimeout = 15 * time.Second

// richPasteMsg carries a clipboard payload that mixed markup with images:
// the images become attachments, the text goes through the normal paste
// path.
//
// uiOwned: dispatched by pasteRichFromClipboard, off pasteImageFromClipboardCmd.
// Routed by active screen instead, a rich paste into a thread's own editor
// could attach its images to the main screen instead, or vice versa.
type richPasteMsg struct {
	uiOwned

	text        string
	attachments []message.Attachment
	skipped     int
}

// pasteImageFromClipboardCmd snapshots the state pasteImageFromClipboard
// needs before returning a tea.Cmd: the closure it wraps runs on the cmd
// goroutine and must not read m off the Update loop.
func (m *UI) pasteImageFromClipboardCmd() tea.Cmd {
	ctx := m.com.Context()
	pasteIdx := m.pasteIdx()
	owner := m
	return func() tea.Msg {
		return pasteImageFromClipboard(ctx, pasteIdx, owner)
	}
}

// pasteImageFromClipboard reads image data from the system clipboard and
// creates an attachment. A rich payload — markup carrying several images,
// as a browser selection does — is handled first. Failing that, a lone
// clipboard image, then clipboard text read as a file path.
func pasteImageFromClipboard(ctx context.Context, pasteIdx int, owner *UI) tea.Msg {
	if msg := pasteRichFromClipboard(ctx, pasteIdx, owner); msg != nil {
		return msg
	}

	imageData, err := clipboard.Read(clipboard.FormatImage)
	if int64(len(imageData)) > common.MaxAttachmentSize {
		return util.InfoMsg{
			Type: util.InfoTypeError,
			Msg:  "File too large, max 5MB",
		}
	}
	name := fmt.Sprintf("paste_%d.png", pasteIdx)
	if err == nil {
		return message.Attachment{
			FilePath: name,
			FileName: name,
			MimeType: mimeOf(imageData),
			Content:  imageData,
		}
	}

	textData, textErr := clipboard.Read(clipboard.FormatText)
	if textErr != nil || len(textData) == 0 {
		return nil // Clipboard is empty or does not contain an image
	}

	path := strings.TrimSpace(string(textData))
	path = strings.ReplaceAll(path, "\\ ", " ")
	if _, statErr := os.Stat(path); statErr != nil {
		// Text that is not a path is what a browser selection looks like
		// once its markup has been dropped for want of a helper.
		return pasteHelperNotice()
	}

	lowerPath := strings.ToLower(path)
	isAllowed := false
	for _, ext := range common.AllowedImageTypes {
		if strings.HasSuffix(lowerPath, ext) {
			isAllowed = true
			break
		}
	}
	if !isAllowed {
		return util.NewInfoMsg("File type is not a supported image format")
	}

	fileInfo, statErr := os.Stat(path)
	if statErr != nil {
		return util.InfoMsg{
			Type: util.InfoTypeError,
			Msg:  fmt.Sprintf("Unable to read file: %v", statErr),
		}
	}
	if fileInfo.Size() > common.MaxAttachmentSize {
		return util.InfoMsg{
			Type: util.InfoTypeError,
			Msg:  "File too large, max 5MB",
		}
	}

	content, readErr := os.ReadFile(path)
	if readErr != nil {
		return util.InfoMsg{
			Type: util.InfoTypeError,
			Msg:  fmt.Sprintf("Unable to read file: %v", readErr),
		}
	}

	return message.Attachment{
		FilePath: path,
		FileName: filepath.Base(path),
		MimeType: mimeOf(content),
		Content:  content,
	}
}

// pasteRichFromClipboard reads the markup flavor of the clipboard and turns
// the images it references into attachments, pairing them with the plain
// text of the same selection. It returns nil when the clipboard holds no
// rich payload, leaving the plain image and file-path paths to run.
func pasteRichFromClipboard(parentCtx context.Context, pasteIdx int, owner *UI) tea.Msg {
	markup, err := clipboard.Read(clipboard.FormatHTML)
	if err != nil || len(markup) == 0 {
		return nil
	}
	srcs := richpaste.ImageSources(markup)
	if len(srcs) == 0 {
		return nil
	}

	var text string
	if data, textErr := clipboard.Read(clipboard.FormatText); textErr == nil {
		text = strings.TrimRight(string(data), "\n")
	}

	// A single image with no text alongside it is the plain image-paste
	// case; the bitmap already on the clipboard beats re-fetching it.
	if len(srcs) == 1 && strings.TrimSpace(text) == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(parentCtx, richPasteTimeout)
	defer cancel()
	images, skipped := richpaste.Resolve(ctx, srcs, richpaste.Options{
		MaxBytes: common.MaxAttachmentSize,
	})
	if len(images) == 0 {
		return nil
	}

	idx := pasteIdx
	attachments := make([]message.Attachment, 0, len(images))
	for i, img := range images {
		name := fmt.Sprintf("paste_%d%s", idx+i, extensionFor(img.MimeType))
		attachments = append(attachments, message.Attachment{
			FilePath: name,
			FileName: name,
			MimeType: img.MimeType,
			Content:  img.Content,
		})
	}

	return richPasteMsg{uiOwned: uiOwned{owner: owner}, text: text, attachments: attachments, skipped: skipped}
}

// handleRichPaste attaches the images of a rich paste and hands the text to
// the regular paste path, so thresholds and bang mode behave as they do for
// any other paste.
func (m *UI) handleRichPaste(msg richPasteMsg) tea.Cmd {
	for _, attachment := range msg.attachments {
		m.editor.attachments.Update(attachment)
	}

	var cmds []tea.Cmd
	if strings.TrimSpace(msg.text) != "" {
		if cmd := m.handlePasteMsg(tea.PasteMsg{Content: msg.text}); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if msg.skipped > 0 {
		cmds = append(cmds, util.ReportWarn(fmt.Sprintf(
			"Skipped %d image(s) that could not be read", msg.skipped,
		)))
	}
	return tea.Batch(cmds...)
}

// pasteHelperNotice explains an inert Ctrl+V when the cause is a missing
// clipboard helper rather than an empty clipboard: without one, a browser
// selection reaches Sennit as bare text and its images are simply gone.
func pasteHelperNotice() tea.Msg {
	missing := clipboard.MissingHTMLHelpers()
	if len(missing) == 0 {
		return nil
	}
	return util.NewWarnMsg(fmt.Sprintf(
		"Install %s to paste images from a browser or document",
		strings.Join(missing, " or "),
	))
}

func extensionFor(mimeType string) string {
	if mimeType == "image/jpeg" {
		return ".jpg"
	}
	return ".png"
}

var pasteRE = regexp.MustCompile(`paste_(\d+)\.(?:txt|png|jpg)`)

func (m *UI) pasteIdx() int {
	result := 0
	for _, at := range m.editor.attachments.List() {
		found := pasteRE.FindStringSubmatch(at.FileName)
		if len(found) == 0 {
			continue
		}
		idx, err := strconv.Atoi(found[1])
		if err == nil {
			result = max(result, idx)
		}
	}
	return result + 1
}
