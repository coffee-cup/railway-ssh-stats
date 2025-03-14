package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
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
	"github.com/charmbracelet/wish/logging"
)

const (
	host                      = "0.0.0.0"
	port                      = "23234"
	statsRefreshInterval      = 10 * time.Second
	realtimeIndicatorDuration = 2 * time.Second
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

// RealtimeIndicatorTickMsg is sent when the realtime indicator should be updated
type RealtimeIndicatorTickMsg struct{}

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

// RealtimeIndicatorCmd sends a message after the indicator duration
func RealtimeIndicatorCmd() tea.Cmd {
	return tea.Tick(realtimeIndicatorDuration, func(time.Time) tea.Msg {
		return RealtimeIndicatorTickMsg{}
	})
}

func teaHandler(s ssh.Session) (tea.Model, []tea.ProgramOption) {
	pty, _, _ := s.Pty()

	renderer := bubbletea.MakeRenderer(s)
	welcomeStyle := renderer.NewStyle().Foreground(lipgloss.Color("5"))
	labelStyle := renderer.NewStyle().PaddingLeft(1)
	valueStyle := renderer.NewStyle().Foreground(lipgloss.Color("10"))
	realtimeStyle := renderer.NewStyle().Foreground(lipgloss.Color("9"))
	quitStyle := renderer.NewStyle().PaddingLeft(1).Foreground(lipgloss.Color("8"))
	titleStyle := renderer.NewStyle().Padding(1).Foreground(lipgloss.Color("5")).Bold(true)

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
		width:              pty.Window.Width,
		height:             pty.Window.Height,
		bg:                 bg,
		welcomeStyle:       welcomeStyle,
		labelStyle:         labelStyle,
		valueStyle:         valueStyle,
		realtimeStyle:      realtimeStyle,
		quitStyle:          quitStyle,
		titleStyle:         titleStyle,
		isWelcome:          true,
		statsChan:          statsChan,
		spinner:            spinner.New(),
		realtimeIndicators: make(map[string]bool),
		realtimeMutex:      &sync.Mutex{},
	}
	return m, options
}

// model represents the application state
type model struct {
	width              int
	height             int
	bg                 string
	welcomeStyle       lipgloss.Style
	labelStyle         lipgloss.Style
	valueStyle         lipgloss.Style
	realtimeStyle      lipgloss.Style
	quitStyle          lipgloss.Style
	titleStyle         lipgloss.Style
	isWelcome          bool
	stats              *stats.PublicStats
	prevStats          *stats.PublicStats
	statsChan          chan *stats.PublicStats
	spinner            spinner.Model
	realtimeIndicators map[string]bool
	realtimeMutex      *sync.Mutex
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
			// Check for changes to highlight with realtime indicators
			if m.stats != nil {
				m.detectChanges(m.stats, msg.Stats)
			}
			m.prevStats = m.stats
			m.stats = msg.Stats
		}
		return m, tea.Batch(
			WaitForStatsCmd(m.statsChan),
			RealtimeIndicatorCmd(),
		)
	case RealtimeIndicatorTickMsg:
		// Clear realtime indicators after the duration
		m.realtimeMutex.Lock()
		m.realtimeIndicators = make(map[string]bool)
		m.realtimeMutex.Unlock()
		return m, nil
	}
	return m, nil
}

// detectChanges identifies which stats have changed and marks them for highlighting
func (m *model) detectChanges(old, new *stats.PublicStats) {
	m.realtimeMutex.Lock()
	defer m.realtimeMutex.Unlock()

	if old.TotalUsers != new.TotalUsers {
		m.realtimeIndicators["totalUsers"] = true
	}
	if old.TotalProjects != new.TotalProjects {
		m.realtimeIndicators["totalProjects"] = true
	}
	if old.TotalServices != new.TotalServices {
		m.realtimeIndicators["totalServices"] = true
	}
	if old.TotalDeploymentsLastMonth != new.TotalDeploymentsLastMonth {
		m.realtimeIndicators["totalDeploymentsLastMonth"] = true
	}
	if old.TotalLogsLastMonth != new.TotalLogsLastMonth {
		m.realtimeIndicators["totalLogsLastMonth"] = true
	}
	if old.TotalRequestsLastMonth != new.TotalRequestsLastMonth {
		m.realtimeIndicators["totalRequestsLastMonth"] = true
	}
}

func (m model) View() string {
	if m.isWelcome {
		return m.welcomeStyle.Render("Welcome to Railway SSH Stats\nPress any key to continue.")
	}

	if m.stats == nil {
		return m.spinner.View() + " " + m.labelStyle.Render("Loading stats...")
	}

	s := m.titleStyle.Render("Railway Stats") + "\n"

	s += formatStatWithRealtime(m, "totalUsers", "Total Users", m.stats.TotalUsers) + "\n"
	s += formatStatWithRealtime(m, "totalProjects", "Total Projects", m.stats.TotalProjects) + "\n"
	s += formatStatWithRealtime(m, "totalServices", "Total Services", m.stats.TotalServices) + "\n"
	s += formatStatWithRealtime(m, "totalDeploymentsLastMonth", "Total Deployments Last Month", m.stats.TotalDeploymentsLastMonth) + "\n"
	s += formatStatWithRealtime(m, "totalLogsLastMonth", "Total Logs Last Month", m.stats.TotalLogsLastMonth) + "\n"
	s += formatStatWithRealtime(m, "totalRequestsLastMonth", "Total Requests Last Month", m.stats.TotalRequestsLastMonth)

	s += "\n\n" + m.quitStyle.Render("Press 'q' to quit\n")

	return s
}

// formatStatWithRealtime formats a stat with the label in the label style and the value in the value style
// If the stat has recently changed, it adds a realtime indicator
func formatStatWithRealtime(m model, key, label string, value int) string {
	m.realtimeMutex.Lock()
	isRealtime := m.realtimeIndicators[key]
	m.realtimeMutex.Unlock()

	valueStr := fmt.Sprintf("%d", value)

	if isRealtime {
		return m.labelStyle.Render(label+": ") + m.valueStyle.Render(valueStr) + " " + m.realtimeStyle.Render("●")
	}

	return m.labelStyle.Render(label+": ") + m.valueStyle.Render(valueStr)
}
