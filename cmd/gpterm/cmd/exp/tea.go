package exp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	markdown "github.com/collinvandyck/go-term-markdown"
	"github.com/collinvandyck/gpterm"
	"github.com/collinvandyck/gpterm/db/query"
	"github.com/spf13/cobra"
)

func TUI() *cobra.Command {
	var logfile string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Run experimental TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			store, err := gpterm.NewStore()
			if err != nil {
				return err
			}
			key, err := store.GetAPIKey(ctx)
			if err != nil {
				return err
			}
			if key == "" {
				fmt.Fprintln(os.Stderr, "No API key has been set. Run this command to set it:")
				fmt.Fprintln(os.Stderr, "")
				fmt.Fprintln(os.Stderr, fmt.Sprintf("%s auth", cmd.Root().Use))
				os.Exit(1)
			}
			gpt, err := gpterm.NewClient(ctx, store)
			if err != nil {
				return err
			}
			defer gpt.Close()
			logger := io.Discard
			if logfile != "" {
				f, err := os.Create(logfile)
				if err != nil {
					return err
				}
				defer f.Close()
				logger = f
			}
			tui := tui{
				store:     store,
				client:    gpt,
				logWriter: logger,
			}
			return tui.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&logfile, "log", "", "log to this file")
	return cmd
}

type tui struct {
	store     *gpterm.Store
	client    *gpterm.Client
	width     int
	height    int
	logWriter io.Writer
}

func (t *tui) Run(ctx context.Context) error {
	t.log("Starting")
	defer t.log("Exiting")
	model, err := t.chatModel()
	if err != nil {
		return err
	}
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func (t *tui) log(msg string, args ...any) {
	fmt.Fprintf(t.logWriter, msg, args...)
	fmt.Fprint(t.logWriter, "\n")
}

func (t *tui) chatModel() (tea.Model, error) {
	return newChatModel(t), nil
}

type chatEntry struct {
	msg query.Message
	err error
}

type chatModel struct {
	t           *tui
	vp          viewport.Model
	ta          textarea.Model
	senderStyle lipgloss.Style
	entries     []chatEntry
	err         error
	readyTerm   bool // when we are ready to render
	readyHist   bool // true when history is loaded
}

// https://github.com/charmbracelet/bubbletea/blob/master/examples/pager/main.go
// https://github.com/charmbracelet/bubbles
func newChatModel(t *tui) chatModel {
	return chatModel{
		t:           t,
		senderStyle: lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
	}
}

func (m chatModel) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.loadHistory(),
	)
}

func (m chatModel) log(msg string, args ...any) {
	m.t.log(msg, args...)
}

func (m chatModel) sendMessage(msg string) tea.Cmd {
	return func() tea.Msg {
		m.log("Sending message")
		ctx, cancel := m.clientContext()
		defer cancel()
		responses, err := m.t.client.Complete(ctx, msg)
		m.log("Got %d message responses err=%v", len(responses), err)
		return messageResponses{responses, err}
	}
}

func (m chatModel) loadHistory() tea.Cmd {
	return func() tea.Msg {
		m.log("Loading history")
		ctx, cancel := m.clientContext()
		defer cancel()
		msgs, err := m.t.store.GetLastMessages(ctx, 50)
		m.log("Loaded %d messages err=%v", len(msgs), err)
		return messageHistory{msgs, err}
	}
}

func (m chatModel) clientContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 1*time.Minute)
}

type messageResponses struct {
	responses []string
	err       error
}

type messageHistory struct {
	messages []query.Message
	err      error
}

func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		taCmd   tea.Cmd
		vpCmd   tea.Cmd
		sendCmd tea.Cmd
	)
	m.ta, taCmd = m.ta.Update(msg)

	switch msg := msg.(type) {

	// we have our dimensions. set up the viewport etc.
	case tea.WindowSizeMsg:
		m.log("Window size changed: %#+v", msg)
		var (
			taWidth  = msg.Width
			taHeight = 3
			vpWidth  = msg.Width
			vpHeight = msg.Height - taHeight - 1
		)
		if !m.readyTerm {
			m.ta = textarea.New()
			m.ta.Placeholder = "Send a message..."
			m.ta.Focus()
			m.ta.Prompt = "┃ "
			m.ta.CharLimit = 280
			m.ta.FocusedStyle.CursorLine = lipgloss.NewStyle() // Remove cursor line styling
			m.ta.ShowLineNumbers = false
			m.vp = viewport.New(vpWidth, vpHeight)
			m.vp.SetContent("No conversations yet")
			m.ta.KeyMap.InsertNewline.SetEnabled(false)
			m.readyTerm = true
		}
		m.vp.Width = vpWidth
		m.vp.Height = vpHeight
		m.ta.SetWidth(taWidth)
		m.ta.SetHeight(taHeight)
		m.updateViewport()

	case messageHistory:
		m.log("Got message history update %d messages err=%v", len(msg.messages), msg.err)
		for _, qm := range msg.messages {
			m.entries = append(m.entries, chatEntry{msg: qm})
		}
		if msg.err != nil {
			m.entries = append(m.entries, chatEntry{err: msg.err})
		}
		m.readyHist = true
		m.updateViewport()

	case messageResponses:
		m.log("Got %d message responses err=%v", len(msg.responses), msg.err)
		for _, mr := range msg.responses {
			m.entries = append(m.entries, chatEntry{
				msg: query.Message{
					Role:    "assistant",
					Content: mr,
				},
			})
		}
		if msg.err != nil {
			m.entries = append(m.entries, chatEntry{err: msg.err})
		}
		m.readyHist = true
		m.updateViewport()

	case tea.KeyMsg:
		switch msg.Type {

		// quit
		case tea.KeyCtrlC, tea.KeyEsc, tea.KeyCtrlD:
			return m, tea.Quit

		// send a message
		case tea.KeyEnter:
			text := strings.TrimSpace(m.ta.Value())
			if text != "" {
				m.entries = append(m.entries, chatEntry{
					msg: query.Message{
						Role:    "user",
						Content: m.ta.Value(),
					},
				})
				sendCmd = m.sendMessage(m.ta.Value())
				m.updateViewport()
			}
			m.ta.Reset()
		}
	default:
		m.vp, vpCmd = m.vp.Update(msg)
	}
	return m, tea.Batch(taCmd, vpCmd, sendCmd)
}

type lineBuilder struct {
	width  int
	buffer bytes.Buffer
}

func (l *lineBuilder) String() string {
	return l.buffer.String()
}

func (l *lineBuilder) Write(line string) {
	const leftpad = 0
	bs := markdown.Render(line, l.width, leftpad)
	//line = wordwrap.String(line, l.width)
	line = string(bs)
	line = strings.TrimSpace(line)
	l.buffer.WriteString(line)
	l.buffer.WriteString("\n")
}

// updates the viewport with the model's current entries
func (m *chatModel) updateViewport() {
	m.log("Updating viewport entries=%d", len(m.entries))
	b := lineBuilder{width: m.vp.Width}
	for i, entry := range m.entries {
		b.Write(m.senderStyle.Render(entry.msg.Role))
		if entry.err != nil {
			b.Write("error: " + entry.err.Error())
		} else {
			b.Write(entry.msg.Content)
		}
		if i < len(m.entries)-1 {
			b.Write("")
		}
	}
	m.vp.SetContent(b.String())
	m.vp.GotoBottom()
}

func (m chatModel) View() string {
	if !m.readyTerm {
		return ""
	}
	var res string
	res += m.vp.View()
	res += "\n"
	res += m.ta.View()
	res += "\n"
	return res
}
