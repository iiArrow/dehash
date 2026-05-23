package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/filepicker"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── styles ────────────────────────────────────────────────────────────────────

var (
	accent = lipgloss.Color("86")  // teal
	dim    = lipgloss.Color("240") // dark gray
	red    = lipgloss.Color("196")
	green  = lipgloss.Color("82")
	orange = lipgloss.Color("214")

	activeInputStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(accent)

	inactiveInputStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("238"))

	labelStyle = lipgloss.NewStyle().
			Foreground(dim).
			Width(10)

	activeLabelStyle = lipgloss.NewStyle().
				Foreground(accent).
				Bold(true).
				Width(10)

	radioOnStyle  = lipgloss.NewStyle().Foreground(accent).Bold(true)
	radioOffStyle = lipgloss.NewStyle().Foreground(dim)

	hintStyle    = lipgloss.NewStyle().Foreground(dim)
	errorStyle   = lipgloss.NewStyle().Foreground(red).Bold(true)
	successStyle = lipgloss.NewStyle().Foreground(green).Bold(true)

	statFoundStyle = lipgloss.NewStyle().Foreground(green).Bold(true)
	statSuspStyle  = lipgloss.NewStyle().Foreground(orange).Bold(true)

	// JSON token styles
	jKey   = lipgloss.NewStyle().Foreground(accent)
	jStr   = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	jNum   = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	jBool  = lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
	jNull  = lipgloss.NewStyle().Foreground(dim)
	jPunct = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
)

// ── state machine ─────────────────────────────────────────────────────────────

type appState int

const (
	stateForm appState = iota
	stateFilePicker
	stateLoading
	stateResults
)

type reqKind int

const (
	reqLookup reqKind = iota
	reqBulk
	reqFile
)

const numFocusFields = 6 // 0=url 1=port 2=reqtype 3=hash/file 4=details 5=send

// ── messages ──────────────────────────────────────────────────────────────────

type responseMsg struct {
	body string
	err  error
}

// ── model ─────────────────────────────────────────────────────────────────────

type model struct {
	state    appState
	width    int
	height   int
	focusIdx int

	urlInput  textinput.Model
	portInput textinput.Model
	hashInput textinput.Model
	fileInput textinput.Model

	reqType reqKind
	details bool

	picker    filepicker.Model
	pickerErr string

	spin spinner.Model

	viewport    viewport.Model
	rawResponse string
	saveMsg     string
	err         error
}

func initialModel() model {
	url := textinput.New()
	url.Placeholder = "localhost"
	url.Width = 28
	url.Focus()

	port := textinput.New()
	port.Placeholder = "8080"
	port.Width = 8

	hash := textinput.New()
	hash.Placeholder = "SHA256 / SHA1 / MD5 / CRC32"
	hash.Width = 56

	file := textinput.New()
	file.Placeholder = "/path/to/hashes.txt"
	file.Width = 48

	fp := filepicker.New()
	fp.CurrentDirectory, _ = os.UserHomeDir()
	fp.Height = 14

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("86"))

	return model{
		state:     stateForm,
		urlInput:  url,
		portInput: port,
		hashInput: hash,
		fileInput: file,
		picker:    fp,
		spin:      sp,
	}
}

// ── tea interface ─────────────────────────────────────────────────────────────

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
		m.height = ws.Height
		if m.state == stateResults {
			m.viewport.Width = ws.Width - 4
			m.viewport.Height = ws.Height - 9
		}
		return m, nil
	}

	switch m.state {
	case stateForm:
		return m.updateForm(msg)
	case stateFilePicker:
		return m.updateFilePicker(msg)
	case stateLoading:
		return m.updateLoading(msg)
	case stateResults:
		return m.updateResults(msg)
	}
	return m, nil
}

// ── form update ───────────────────────────────────────────────────────────────

func (m model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "tab", "down":
			m = blurAll(m)
			m.focusIdx = (m.focusIdx + 1) % numFocusFields
			m = applyFocus(m)
			return m, textinput.Blink

		case "shift+tab", "up":
			m = blurAll(m)
			m.focusIdx = (m.focusIdx - 1 + numFocusFields) % numFocusFields
			m = applyFocus(m)
			return m, textinput.Blink

		case "left":
			if m.focusIdx == 2 {
				if m.reqType > 0 {
					m.reqType--
					m = applyFocus(m)
				}
			} else if m.focusIdx == 4 {
				m.details = false
			}
			return m, nil

		case "right":
			if m.focusIdx == 2 {
				if int(m.reqType) < 2 {
					m.reqType++
					m = applyFocus(m)
				}
			} else if m.focusIdx == 4 {
				m.details = true
			}
			return m, nil

		case " ":
			if m.focusIdx == 4 {
				m.details = !m.details
			}
			return m, nil

		case "enter":
			switch m.focusIdx {
			case 2:
				m.reqType = (m.reqType + 1) % 3
				m = applyFocus(m)
				return m, nil
			case 3:
				if m.reqType != reqLookup {
					m.state = stateFilePicker
					m.pickerErr = ""
					return m, m.picker.Init()
				}
				// fall through to advance focus
			case 4:
				m.details = !m.details
				return m, nil
			case 5:
				return m.sendRequest()
			}
			// advance on Enter for text fields
			m = blurAll(m)
			m.focusIdx = (m.focusIdx + 1) % numFocusFields
			m = applyFocus(m)
			return m, textinput.Blink
		}
	}

	// route key input to the active text input
	var cmd tea.Cmd
	switch m.focusIdx {
	case 0:
		m.urlInput, cmd = m.urlInput.Update(msg)
	case 1:
		m.portInput, cmd = m.portInput.Update(msg)
	case 3:
		if m.reqType == reqLookup {
			m.hashInput, cmd = m.hashInput.Update(msg)
		} else {
			m.fileInput, cmd = m.fileInput.Update(msg)
		}
	}
	return m, cmd
}

func blurAll(m model) model {
	m.urlInput.Blur()
	m.portInput.Blur()
	m.hashInput.Blur()
	m.fileInput.Blur()
	return m
}

func applyFocus(m model) model {
	switch m.focusIdx {
	case 0:
		m.urlInput.Focus()
	case 1:
		m.portInput.Focus()
	case 3:
		if m.reqType == reqLookup {
			m.hashInput.Focus()
		} else {
			m.fileInput.Focus()
		}
	}
	return m
}

// ── file picker update ────────────────────────────────────────────────────────

func (m model) updateFilePicker(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		if key.String() == "esc" || key.String() == "ctrl+c" {
			m.state = stateForm
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.picker, cmd = m.picker.Update(msg)

	if ok, path := m.picker.DidSelectFile(msg); ok {
		m.fileInput.SetValue(path)
		m.state = stateForm
		return m, nil
	}
	if ok, path := m.picker.DidSelectDisabledFile(msg); ok {
		m.pickerErr = path + " — pick any text file"
	}

	return m, cmd
}

// ── loading update ────────────────────────────────────────────────────────────

func (m model) updateLoading(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case responseMsg:
		if msg.err != nil {
			m.err = msg.err
			m.state = stateForm
			return m, nil
		}
		m.rawResponse = msg.body
		vw := m.width - 4
		vh := m.height - 9
		if vw < 20 {
			vw = 80
		}
		if vh < 5 {
			vh = 20
		}
		m.viewport = viewport.New(vw, vh)
		m.viewport.SetContent(colorizeJSON(msg.body))
		m.saveMsg = ""
		m.state = stateResults
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

// ── results update ────────────────────────────────────────────────────────────

func (m model) updateResults(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "b", "esc":
			m.state = stateForm
			m.err = nil
			return m, nil
		case "s":
			name, err := saveResponse(m.rawResponse)
			if err != nil {
				m.saveMsg = errorStyle.Render("save failed: " + err.Error())
			} else {
				m.saveMsg = successStyle.Render("saved → " + name)
			}
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

func (m model) sendRequest() (model, tea.Cmd) {
	m.err = nil
	m.state = stateLoading

	host := strings.TrimSpace(m.urlInput.Value())
	if host == "" {
		host = "localhost"
	}
	port := strings.TrimSpace(m.portInput.Value())
	if port == "" {
		port = "8080"
	}
	base := "http://" + host + ":" + port

	kind := m.reqType
	details := m.details
	hash := strings.TrimSpace(m.hashInput.Value())
	filePath := strings.TrimSpace(m.fileInput.Value())

	return m, tea.Batch(
		m.spin.Tick,
		func() tea.Msg {
			body, err := doRequest(base, kind, details, hash, filePath)
			return responseMsg{body: body, err: err}
		},
	)
}

func doRequest(base string, kind reqKind, details bool, hash, filePath string) (string, error) {
	client := &http.Client{Timeout: 120 * time.Second}

	var resp *http.Response
	var err error

	switch kind {
	case reqLookup:
		payload, _ := json.Marshal(map[string]any{"hash": hash, "details": details})
		resp, err = client.Post(base+"/lookup", "application/json", bytes.NewReader(payload))

	case reqBulk:
		data, readErr := os.ReadFile(filePath)
		if readErr != nil {
			return "", fmt.Errorf("read file: %w", readErr)
		}
		var hashes []string
		for _, line := range strings.Split(string(data), "\n") {
			if h := strings.TrimSpace(line); h != "" {
				hashes = append(hashes, h)
			}
		}
		payload, _ := json.Marshal(map[string]any{"hashes": hashes, "details": details})
		resp, err = client.Post(base+"/bulk", "application/json", bytes.NewReader(payload))

	case reqFile:
		data, readErr := os.ReadFile(filePath)
		if readErr != nil {
			return "", fmt.Errorf("read file: %w", readErr)
		}
		detStr := "false"
		if details {
			detStr = "true"
		}
		req, _ := http.NewRequest("POST", base+"/file?details="+detStr, bytes.NewReader(data))
		req.Header.Set("Content-Type", "text/plain")
		resp, err = client.Do(req)
	}

	if err != nil {
		return "", fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("server %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil {
		return string(raw), nil
	}
	return pretty.String(), nil
}

func saveResponse(data string) (string, error) {
	name := "response_" + time.Now().Format("20060102_150405") + ".json"
	return name, os.WriteFile(name, []byte(data), 0644)
}

// ── JSON colorizer ────────────────────────────────────────────────────────────

func colorizeJSON(input string) string {
	var out strings.Builder
	runes := []rune(input)
	n := len(runes)
	i := 0

	for i < n {
		c := runes[i]

		// whitespace / newlines — pass through
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			out.WriteRune(c)
			i++
			continue
		}

		// string token
		if c == '"' {
			j := i + 1
			for j < n {
				if runes[j] == '\\' {
					j += 2
					continue
				}
				if runes[j] == '"' {
					j++
					break
				}
				j++
			}
			s := string(runes[i:j])
			// peek past whitespace for ':'
			k := j
			for k < n && (runes[k] == ' ' || runes[k] == '\t') {
				k++
			}
			if k < n && runes[k] == ':' {
				out.WriteString(jKey.Render(s))
			} else {
				out.WriteString(jStr.Render(s))
			}
			i = j
			continue
		}

		// number
		if (c >= '0' && c <= '9') || (c == '-' && i+1 < n && runes[i+1] >= '0' && runes[i+1] <= '9') {
			j := i + 1
			for j < n && (runes[j] >= '0' && runes[j] <= '9' || runes[j] == '.' ||
				runes[j] == 'e' || runes[j] == 'E' || runes[j] == '+') {
				j++
			}
			out.WriteString(jNum.Render(string(runes[i:j])))
			i = j
			continue
		}

		// true
		if c == 't' && i+3 < n && string(runes[i:i+4]) == "true" {
			out.WriteString(jBool.Render("true"))
			i += 4
			continue
		}
		// false
		if c == 'f' && i+4 < n && string(runes[i:i+5]) == "false" {
			out.WriteString(jBool.Render("false"))
			i += 5
			continue
		}
		// null
		if c == 'n' && i+3 < n && string(runes[i:i+4]) == "null" {
			out.WriteString(jNull.Render("null"))
			i += 4
			continue
		}

		// punctuation: { } [ ] : ,
		out.WriteString(jPunct.Render(string(c)))
		i++
	}

	return out.String()
}

// ── views ─────────────────────────────────────────────────────────────────────

func (m model) View() string {
	switch m.state {
	case stateForm:
		return m.viewForm()
	case stateFilePicker:
		return m.viewFilePicker()
	case stateLoading:
		return m.viewLoading()
	case stateResults:
		return m.viewResults()
	}
	return ""
}

func (m model) titleBar(subtitle string) string {
	w := m.width
	if w < 40 {
		w = 80
	}
	bar := lipgloss.NewStyle().
		Background(lipgloss.Color("236")).
		Foreground(accent).
		Bold(true).
		Width(w).
		Padding(0, 2)
	return bar.Render("dehash  ·  "+subtitle) + "\n"
}

func (m model) viewForm() string {
	var b strings.Builder

	b.WriteString(m.titleBar("NSRL Hash Lookup"))
	b.WriteString("\n")

	// server + port: JoinHorizontal centers the 1-line label against the 3-line bordered box
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Center,
		fmtLabel("Server", m.focusIdx == 0),
		fmtInput(m.urlInput, m.focusIdx == 0),
		"   ",
		fmtLabel("Port", m.focusIdx == 1),
		fmtInput(m.portInput, m.focusIdx == 1),
	))
	b.WriteString("\n\n")

	// request type — radio buttons, no borders, single-line is fine
	b.WriteString(fmtLabel("Type", m.focusIdx == 2))
	types := []string{"lookup", "bulk", "file"}
	for i, t := range types {
		if i == int(m.reqType) {
			b.WriteString(radioOnStyle.Render("● " + t))
		} else {
			b.WriteString(radioOffStyle.Render("○ " + t))
		}
		if i < len(types)-1 {
			b.WriteString("   ")
		}
	}
	if m.focusIdx == 2 {
		b.WriteString("  " + hintStyle.Render("← →"))
	}
	b.WriteString("\n\n")

	// hash or file input — JoinHorizontal same reason as server row
	if m.reqType == reqLookup {
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Center,
			fmtLabel("Hash", m.focusIdx == 3),
			fmtInput(m.hashInput, m.focusIdx == 3),
		))
	} else {
		cols := []string{
			fmtLabel("File", m.focusIdx == 3),
			fmtInput(m.fileInput, m.focusIdx == 3),
		}
		if m.focusIdx == 3 {
			cols = append(cols, "  "+hintStyle.Render("enter: browse"))
		}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Center, cols...))
	}
	b.WriteString("\n\n")

	// details — radio buttons, single-line
	b.WriteString(fmtLabel("Details", m.focusIdx == 4))
	if m.details {
		b.WriteString(radioOnStyle.Render("● yes") + "   " + radioOffStyle.Render("○ no"))
	} else {
		b.WriteString(radioOffStyle.Render("○ yes") + "   " + radioOnStyle.Render("● no"))
	}
	if m.focusIdx == 4 {
		b.WriteString("  " + hintStyle.Render("← → or space"))
	}
	b.WriteString("\n\n")

	// send button — MarginLeft keeps all 3 lines indented
	var sendBtn string
	if m.focusIdx == 5 {
		sendBtn = activeInputStyle.MarginLeft(12).Render(
			lipgloss.NewStyle().Foreground(accent).Bold(true).Padding(0, 2).Render("Send Request"),
		)
	} else {
		sendBtn = inactiveInputStyle.MarginLeft(12).Render(
			lipgloss.NewStyle().Foreground(dim).Padding(0, 2).Render("Send Request"),
		)
	}
	b.WriteString(sendBtn + "\n")

	// error
	if m.err != nil {
		b.WriteString("\n" + errorStyle.Render("  ✗ "+m.err.Error()) + "\n")
	}

	// hints
	b.WriteString("\n" + hintStyle.Render("  tab ↑↓ navigate   ← → change   space toggle   enter confirm   ctrl+c quit"))

	return b.String()
}

func fmtLabel(text string, focused bool) string {
	s := text + ":"
	if focused {
		return activeLabelStyle.Render(s) + "  "
	}
	return labelStyle.Render(s) + "  "
}

func fmtInput(ti textinput.Model, focused bool) string {
	if focused {
		return activeInputStyle.Render(ti.View())
	}
	return inactiveInputStyle.Render(ti.View())
}

func (m model) viewFilePicker() string {
	var b strings.Builder
	b.WriteString(m.titleBar("Select File"))
	b.WriteString("\n")
	b.WriteString(m.picker.View())
	if m.pickerErr != "" {
		b.WriteString("\n" + errorStyle.Render("  "+m.pickerErr))
	}
	b.WriteString("\n" + hintStyle.Render("  ↑↓ navigate   enter select   esc cancel"))
	return b.String()
}

func (m model) viewLoading() string {
	return "\n\n  " + m.spin.View() + "  Sending request…\n\n" +
		hintStyle.Render("  ctrl+c to quit")
}

func (m model) viewResults() string {
	var b strings.Builder

	b.WriteString(m.titleBar("Response"))

	if stats := extractStats(m.rawResponse); stats != "" {
		b.WriteString("\n  " + stats + "\n")
	}
	b.WriteString("\n")

	// viewport with accent border
	vp := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent)
	b.WriteString(vp.Render(m.viewport.View()))
	b.WriteString("\n")

	// save message
	if m.saveMsg != "" {
		b.WriteString("  " + m.saveMsg + "\n")
	}

	// scroll %
	pct := 100
	if total := m.viewport.TotalLineCount(); total > 0 {
		pct = int(float64(m.viewport.YOffset+m.viewport.Height) / float64(total) * 100)
		if pct > 100 {
			pct = 100
		}
	}
	b.WriteString(hintStyle.Render(fmt.Sprintf("  ↑↓ pgup pgdn scroll (%d%%)   s save   b back   ctrl+c quit", pct)))

	return b.String()
}

func extractStats(raw string) string {
	var data map[string]any
	if json.Unmarshal([]byte(raw), &data) != nil {
		return ""
	}

	var parts []string

	if status, ok := data["status"].(string); ok {
		switch status {
		case "found":
			parts = append(parts, statFoundStyle.Render("● found"))
		case "not_found":
			parts = append(parts, statSuspStyle.Render("● not found"))
		}
	}
	if fc, ok := data["found_count"].(float64); ok {
		parts = append(parts, statFoundStyle.Render(fmt.Sprintf("known-good: %.0f", fc)))
	}
	if nf, ok := data["not_found"].([]any); ok {
		parts = append(parts, statSuspStyle.Render(fmt.Sprintf("suspicious: %d", len(nf))))
	}

	return strings.Join(parts, "  │  ")
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
