package model

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

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

func (m *UI) openEditor(value string) tea.Cmd {
	tmpfile, err := os.CreateTemp("", "msg_*.md")
	if err != nil {
		return util.ReportError(err)
	}
	tmpPath := tmpfile.Name()
	defer tmpfile.Close()
	if _, err := tmpfile.WriteString(value); err != nil {
		return util.ReportError(err)
	}
	cmd, err := editor.Command(
		brand.Slug,
		tmpPath,
		editor.AtPosition(
			m.editor.textarea.Line()+1,
			m.editor.textarea.Column()+1,
		),
	)
	if err != nil {
		return util.ReportError(err)
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer func() {
			_ = os.Remove(tmpPath)
		}()

		if err != nil {
			return util.NewErrorMsg(err)
		}
		content, err := os.ReadFile(tmpPath)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		if len(content) == 0 {
			return util.NewWarnMsg("Message is empty")
		}
		return openEditorMsg{
			Text: strings.TrimSpace(string(content)),
		}
	})
}

// setEditorPrompt configures the textarea prompt function based on whether
// yolo mode or bang mode is enabled.
func (m *UI) setEditorPrompt(yolo bool) {
	if m.editor.bangMode {
		m.editor.textarea.SetPromptFunc(4, m.bangPromptFunc)
		return
	}
	if yolo {
		m.editor.textarea.SetPromptFunc(4, m.yoloPromptFunc)
		return
	}
	m.editor.textarea.SetPromptFunc(2, m.normalPromptFunc)
}

// normalPromptFunc keeps the prompt width as whitespace so multiline text
// stays aligned without visible prompt markers.
func (m *UI) normalPromptFunc(info textarea.PromptInfo) string {
	return "  "
}

// yoloPromptFunc returns the yolo mode editor prompt style with warning icon
// and colored dots.
func (m *UI) yoloPromptFunc(info textarea.PromptInfo) string {
	t := m.com.Styles
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
func (m *UI) bangPromptFunc(info textarea.PromptInfo) string {
	t := m.com.Styles
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
type fileCompletionMsg struct {
	absPath    string
	attachment message.Attachment
}

// insertFileCompletion inserts the selected file path into the textarea,
// replacing the @query, and adds the file as an attachment.
func (m *UI) insertFileCompletion(path string) tea.Cmd {
	prevHeight := m.editor.textarea.Height()
	if !m.editor.insertCompletionText(path) {
		return nil
	}
	heightCmd := m.handleTextareaHeightChange(prevHeight)

	// Snapshot session state up front: fileCmd's closure runs on the cmd
	// goroutine and must not read or write m.sess off the Update loop.
	hasSession := m.hasSession()
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

		// Add file as attachment.
		content, err := os.ReadFile(path)
		if err != nil {
			// If it fails, let the LLM handle it later.
			return nil
		}

		return fileCompletionMsg{
			absPath: absPath,
			attachment: message.Attachment{
				FilePath: path,
				FileName: filepath.Base(path),
				MimeType: mimeOf(content),
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
	if !m.editor.insertCompletionText(displayText) {
		return nil
	}
	heightCmd := m.handleTextareaHeightChange(prevHeight)

	resourceCmd := func() tea.Msg {
		contents, err := m.com.Workspace.ReadMCPResource(
			m.com.Context(),
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

// mimeOf detects the MIME type of the given content.
func mimeOf(content []byte) string {
	mimeBufferSize := min(512, len(content))
	return http.DetectContentType(content[:mimeBufferSize])
}

var readyPlaceholders = [...]string{
	"Ready!",
	"Ready...",
	"Ready?",
	"Ready for instructions",
}

var workingPlaceholders = [...]string{
	"Working!",
	"Working...",
	"Brrrrr...",
	"Prrrrrrrr...",
	"Processing...",
	"Thinking...",
}

// randomizePlaceholders selects random placeholder text for the textarea's
// ready and working states.
func (m *UI) randomizePlaceholders() {
	m.editor.workingPlaceholder = workingPlaceholders[rand.Intn(len(workingPlaceholders))]
	m.editor.readyPlaceholder = readyPlaceholders[rand.Intn(len(readyPlaceholders))]
}

// checkBangModeAfterPaste engages bang mode when pasted text starts with
// optional whitespace followed by "!". It strips the prefix and adjusts
// the cursor, mirroring the keypress bang-mode entry logic.
func (m *UI) checkBangModeAfterPaste() {
	if m.editor.bangMode {
		return
	}
	val := m.editor.textarea.Value()
	trimmed := strings.TrimLeftFunc(val, unicode.IsSpace)
	if !strings.HasPrefix(trimmed, "!") {
		return
	}
	m.editor.bangMode = true
	m.editor.bangWasEmpty = true
	stripped := trimmed[1:]
	m.editor.textarea.SetValue(stripped)
	col := m.editor.textarea.Column()
	m.editor.textarea.SetCursorColumn(max(0, col-(len(val)-len(stripped))))
	m.setEditorPrompt(m.yoloModeCached())
}

// handlePasteMsg handles a paste message.
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
		return func() tea.Msg {
			content := []byte(msg.Content)
			if int64(len(content)) > common.MaxAttachmentSize {
				// A tea.Cmd here would be delivered as a message and
				// dropped: this command's return value is the message.
				return util.NewWarnMsg("Paste is too big (>5mb)")
			}
			name := fmt.Sprintf("paste_%d.txt", m.pasteIdx())
			return common.AttachmentFromBytes(name, name, content)
		}
	}

	// Attempt to parse pasted content as file paths. If possible to parse,
	// all files exist and are valid, add as attachments.
	// Otherwise, paste as text.
	paths := fsext.ParsePastedFiles(msg.Content)
	allExistsAndValid := func() bool {
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
	if !allExistsAndValid() {
		prevHeight := m.editor.textarea.Height()
		cmd := m.updateTextareaWithPrevHeight(msg, prevHeight)
		m.checkBangModeAfterPaste()
		return cmd
	}

	var cmds []tea.Cmd
	for _, path := range paths {
		cmds = append(cmds, m.handleFilePathPaste(path))
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

// handleFilePathPaste handles a pasted file path.
func (m *UI) handleFilePathPaste(path string) tea.Cmd {
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
type richPasteMsg struct {
	text        string
	attachments []message.Attachment
	skipped     int
}

// pasteImageFromClipboard reads image data from the system clipboard and
// creates an attachment. A rich payload — markup carrying several images,
// as a browser selection does — is handled first. Failing that, a lone
// clipboard image, then clipboard text read as a file path.
func (m *UI) pasteImageFromClipboard() tea.Msg {
	if msg := m.pasteRichFromClipboard(); msg != nil {
		return msg
	}

	imageData, err := clipboard.Read(clipboard.FormatImage)
	if int64(len(imageData)) > common.MaxAttachmentSize {
		return util.InfoMsg{
			Type: util.InfoTypeError,
			Msg:  "File too large, max 5MB",
		}
	}
	name := fmt.Sprintf("paste_%d.png", m.pasteIdx())
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
func (m *UI) pasteRichFromClipboard() tea.Msg {
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

	ctx, cancel := context.WithTimeout(m.com.Context(), richPasteTimeout)
	defer cancel()
	images, skipped := richpaste.Resolve(ctx, srcs, richpaste.Options{
		MaxBytes: common.MaxAttachmentSize,
	})
	if len(images) == 0 {
		return nil
	}

	idx := m.pasteIdx()
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

	return richPasteMsg{text: text, attachments: attachments, skipped: skipped}
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
