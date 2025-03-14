package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "embed"

	"github.com/coffee-cup/railway-stats-ssh/stats"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	"github.com/charmbracelet/wish/bubbletea"
	"github.com/charmbracelet/wish/elapsed"
	"github.com/charmbracelet/wish/logging"
)

const (
	host                 = "0.0.0.0"
	port                 = "23234"
	statsRefreshInterval = 10 * time.Second
)

//go:embed banner.txt
var banner string

// Global broadcaster instance
var broadcaster *stats.StatsBroadcaster

func main() {
	// Initialize the broadcaster with refresh interval
	broadcaster = stats.NewStatsBroadcaster(statsRefreshInterval)
	defer broadcaster.Shutdown()

	s, err := wish.NewServer(
		wish.WithAddress(net.JoinHostPort(host, port)),
		wish.WithHostKeyPath(".ssh/id_ed25519"),
		wish.WithBannerHandler(func(ctx ssh.Context) string {
			return fmt.Sprintf(banner+"\n\n", ctx.User())
		}),
		wish.WithMiddleware(
			bubbletea.Middleware(teaHandler),
			activeterm.Middleware(),
			logging.Middleware(),
			elapsed.Middleware(),
		),
	)
	if err != nil {
		log.Error("Could not start server", "error", err)
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	log.Info("Starting SSH server", "host", host, "port", port)
	go func() {
		if err = s.ListenAndServe(); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			log.Error("Could not start server", "error", err)
			done <- nil
		}
	}()

	<-done
	log.Info("Stopping SSH server")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := s.Shutdown(shutdownCtx); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
		log.Error("Could not stop server", "error", err)
	}
}

// StatsMsg is a message containing the latest stats
type StatsMsg struct {
	Stats *stats.PublicStats
}

// WaitForStatsCmd waits for stats updates from the channel
func WaitForStatsCmd(ch chan *stats.PublicStats) tea.Cmd {
	return func() tea.Msg {
		stats, ok := <-ch
		if !ok {
			return nil // Channel closed
		}
		return StatsMsg{Stats: stats}
	}
}

func teaHandler(s ssh.Session) (tea.Model, []tea.ProgramOption) {
	pty, _, _ := s.Pty()

	renderer := bubbletea.MakeRenderer(s)
	txtStyle := renderer.NewStyle().Foreground(lipgloss.Color("10"))
	quitStyle := renderer.NewStyle().Foreground(lipgloss.Color("8"))
	titleStyle := renderer.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)

	bg := "light"
	if renderer.HasDarkBackground() {
		bg = "dark"
	}

	options := []tea.ProgramOption{
		tea.WithMouseCellMotion(),
		tea.WithMouseAllMotion(),
	}

	// Subscribe to stats updates
	statsChan := broadcaster.Subscribe()

	m := model{
		width:      pty.Window.Width,
		height:     pty.Window.Height,
		bg:         bg,
		txtStyle:   txtStyle,
		quitStyle:  quitStyle,
		titleStyle: titleStyle,
		isWelcome:  true,
		statsChan:  statsChan,
		spinner:    spinner.New(),
	}
	return m, options
}

// model represents the application state
type model struct {
	width      int
	height     int
	bg         string
	txtStyle   lipgloss.Style
	quitStyle  lipgloss.Style
	titleStyle lipgloss.Style
	isWelcome  bool
	stats      *stats.PublicStats
	statsChan  chan *stats.PublicStats
	spinner    spinner.Model
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		WaitForStatsCmd(m.statsChan),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.isWelcome {
		if msg, ok := msg.(tea.KeyMsg); ok {
			key := msg.String()
			if key == "q" || key == "ctrl+c" {
				broadcaster.Unsubscribe(m.statsChan)
				return m, tea.Quit
			} else {
				m.isWelcome = false
				return m, tea.EnterAltScreen
			}
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			broadcaster.Unsubscribe(m.statsChan)
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case StatsMsg:
		if msg.Stats != nil {
			m.stats = msg.Stats
		}
		return m, WaitForStatsCmd(m.statsChan)
	}
	return m, nil
}

func (m model) View() string {
	if m.isWelcome {
		return m.txtStyle.Render("Welcome to Railway SSH Stats\nPress any key to continue.")
	}

	if m.stats == nil {
		return m.spinner.View() + " " + m.txtStyle.Render("Loading stats...")
	}

	s := m.titleStyle.Render("Railway Public Stats") + "\n\n"

	s += m.txtStyle.Render(fmt.Sprintf("Total Users: %d\n", m.stats.TotalUsers))
	s += m.txtStyle.Render(fmt.Sprintf("Total Projects: %d\n", m.stats.TotalProjects))
	s += m.txtStyle.Render(fmt.Sprintf("Total Services: %d\n", m.stats.TotalServices))
	s += m.txtStyle.Render(fmt.Sprintf("Total Deployments Last Month: %d\n", m.stats.TotalDeploymentsLastMonth))
	s += m.txtStyle.Render(fmt.Sprintf("Total Logs Last Month: %d\n", m.stats.TotalLogsLastMonth))
	s += m.txtStyle.Render(fmt.Sprintf("Total Requests Last Month: %d\n", m.stats.TotalRequestsLastMonth))

	s += "\n" + m.spinner.View() + " " + m.txtStyle.Render("Updating every 10s")
	s += "\n\n" + m.quitStyle.Render("Press 'q' to quit\n")

	return s
}
