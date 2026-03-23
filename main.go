package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

const (
	DefaultMsgLimit = 3000
	RetryMsgLimit   = 2000
	FinalizeWait    = 8 * time.Second
	AgentMarker     = "✦"
)

var (
	reANSI   = regexp.MustCompile(`[\x1b\x9b][[()#;?]*(?:[0-9]{1,4}(?:;[0-9]{0,4})*)?[0-9A-ORZcf-nqry=><]`)
	spinners = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "⠋", "⠓", "⠒", "⠚", "⠝"}
)

func stripANSI(str string) string {
	return reANSI.ReplaceAllString(str, "")
}

type AgentSession struct {
	mu             sync.RWMutex
	ptyFile        *os.File
	api            *slack.Client
	channelID      string
	sessionName    string
	latestReplyTS  string
	originTS       string
	currentBuffer  string
	currentMarker  string
	sentOffset     int
	commandSentAt  time.Time
	isProcessing   bool
	isReady        bool
	readyWait      chan struct{}
	done           chan struct{}
}

func NewAgentSession(api *slack.Client, channelID string) (*AgentSession, error) {
	sessionName := fmt.Sprintf("gemini_%s", strings.ReplaceAll(channelID, "-", "_"))
	log.Printf("[Session] 고신뢰성 엔진 기동: %s", sessionName)

	exec.Command("tmux", "kill-session", "-t", sessionName).Run()
	geminiPath := os.Getenv("GEMINI_BIN_PATH")
	if geminiPath == "" {
		geminiPath = "gemini"
	}

	startCmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName,
		fmt.Sprintf("LANG=C.UTF-8 LC_ALL=C.UTF-8 TERM=xterm-256color %s --approval-mode=yolo", geminiPath))
	if err := startCmd.Run(); err != nil {
		return nil, err
	}

	exec.Command("tmux", "set-option", "-t", sessionName, "status", "off").Run()
	exec.Command("tmux", "resize-window", "-t", sessionName, "-x", "100", "-y", "30").Run()

	attachCmd := exec.Command("tmux", "attach-session", "-t", sessionName)
	f, err := pty.Start(attachCmd)
	if err != nil {
		return nil, err
	}
	pty.Setsize(f, &pty.Winsize{Rows: 30, Cols: 100})

	session := &AgentSession{
		ptyFile:     f,
		api:         api,
		channelID:   channelID,
		sessionName: sessionName,
		readyWait:   make(chan struct{}),
		done:        make(chan struct{}),
	}

	go session.waitReady()
	go session.syncLoop()
	return session, nil
}

func (s *AgentSession) waitReady() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	stableCount := 0
	for {
		select {
		case <-time.After(120 * time.Second):
			s.markReady()
			return
		case <-ticker.C:
			out, _ := exec.Command("tmux", "capture-pane", "-p", "-t", s.sessionName).Output()
			content := string(out)
			isBusy := strings.Contains(content, "Waiting for MCP") || strings.Contains(content, "initializing") || strings.Contains(content, "Queued")
			hasPrompt := strings.Contains(content, "> ") || strings.Contains(content, "Type your message")
			if hasPrompt && !isBusy {
				stableCount++
				if stableCount >= 4 {
					log.Printf("[Session] %s 기동 완료", s.sessionName)
					s.markReady()
					return
				}
			} else {
				stableCount = 0
			}
		}
	}
}

func (s *AgentSession) markReady() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isReady {
		s.isReady = true
		close(s.readyWait)
	}
}

func (s *AgentSession) syncLoop() {
	ticker := time.NewTicker(1200 * time.Millisecond)
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.adaptiveSync()
		}
	}
}

func (s *AgentSession) finalize() {
	s.api.AddReaction("white_check_mark", slack.NewRefToMessage(s.channelID, s.originTS))
	s.api.RemoveReaction("hourglass_flowing_sand", slack.NewRefToMessage(s.channelID, s.originTS))
	s.latestReplyTS = ""
	s.isProcessing = false
}

func (s *AgentSession) adaptiveSync() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.latestReplyTS == "" || !s.isProcessing {
		return
	}

	out, err := exec.Command("tmux", "capture-pane", "-pS", "-", "-t", s.sessionName).Output()
	if err != nil {
		log.Printf("[Sync] tmux capture error: %v", err)
		return
	}
	content := stripANSI(string(out))

	if s.currentMarker == "" {
		return
	}
	cmdStartIdx := strings.LastIndex(content, s.currentMarker)
	if cmdStartIdx == -1 {
		return
	}

	currentContent := content[cmdStartIdx:]
	markerIdx := strings.Index(currentContent, AgentMarker)
	if markerIdx == -1 {
		if isFinished(currentContent) && time.Since(s.commandSentAt) > 10*time.Second {
			log.Printf("[Sync] 답변 없이 종료됨 (에러 가능성)")
			s.finalize()
		}
		return
	}

	body := currentContent[markerIdx:]
	fullBody := cleanResponse(body)
	runes := []rune(fullBody)

	totalSent := s.sentOffset + len([]rune(s.currentBuffer))

	if totalSent >= len(runes) && isFinished(currentContent) {
		if time.Since(s.commandSentAt) > FinalizeWait {
			log.Printf("[Sync] 명령어 처리 최종 완료 감지 (Total Rune: %d)", len(runes))
			s.finalize()
			return
		}
	}

	if s.sentOffset > len(runes) {
		s.sentOffset = len(runes)
	}
	remaining := runes[s.sentOffset:]

	limit := DefaultMsgLimit
	isOverLimit := len(remaining) > limit
	var chunk string

	for retry := 0; retry < 2; retry++ {
		currentLimit := limit
		if retry > 0 {
			currentLimit = RetryMsgLimit
		}

		if len(remaining) > currentLimit {
			chunk = string(remaining[:currentLimit])
			isOverLimit = true
		} else {
			chunk = string(remaining)
			isOverLimit = false
		}

		if chunk == s.currentBuffer {
			return
		}

		display := fmt.Sprintf("```\n%s\n```", chunk)
		if isOverLimit {
			display += "\n*(답변이 너무 길어 다음 메시지로 계속됩니다...)*"
		}

		_, _, _, err = s.api.UpdateMessage(s.channelID, s.latestReplyTS, slack.MsgOptionText(display, false))
		if err != nil {
			if strings.Contains(err.Error(), "msg_too_long") {
				log.Printf("[Warn] Message too long. Retrying with limit=%d...", RetryMsgLimit)
				limit = RetryMsgLimit
				continue
			}
			log.Printf("[Sync] UpdateMessage error: %v", err)
			return
		}

		log.Printf("[Sync] SUCCESS | Runes: %d", len(chunk))
		s.currentBuffer = chunk

		if isOverLimit {
			s.handleChaining()
		}
		break
	}
}

func (s *AgentSession) handleChaining() {
	actualSent := len([]rune(s.currentBuffer))
	log.Printf("[Chain] 임계치 도달 -> 다음 메시지 생성 시도")

	prevTS := s.latestReplyTS
	s.latestReplyTS = "" // 잠시 업데이트 차단

	_, nextTS, err := s.api.PostMessage(s.channelID,
		slack.MsgOptionText("```\n(답변 계속됨...)\n```", false),
		slack.MsgOptionTS(s.originTS))

	if err == nil {
		s.sentOffset += actualSent
		s.latestReplyTS = nextTS
		s.currentBuffer = ""
		log.Printf("[Chain] SUCCESS | New TS: %s", nextTS)
	} else {
		log.Printf("[Chain] FAIL: %v. Reverting TS.", err)
		s.latestReplyTS = prevTS
	}
}

func cleanResponse(raw string) string {
	lines := strings.Split(raw, "\n")
	var result []string

	for _, line := range lines {
		if shouldSkipLine(line) {
			continue
		}
		result = append(result, strings.TrimRight(line, " \r\t"))
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

func shouldSkipLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	for _, s := range spinners {
		if strings.Contains(line, s) {
			return true
		}
	}

	noisePatterns := []string{
		"▀▀▀▀", "▄▄▄▄", "────────", "YOLO", "Ctrl+Y",
		"MCP servers", "GEMINI.md file", "workspace (/",
		"Auto (Gemini", "CMD_BOUNDARY", "(esc to cancel",
		"Type your message",
	}

	for _, p := range noisePatterns {
		if strings.Contains(line, p) {
			return true
		}
	}

	if (strings.HasPrefix(trimmed, ">") && len(trimmed) < 4) ||
		strings.HasPrefix(trimmed, "*   Type your message") ||
		strings.HasPrefix(trimmed, "Type your message or @path/to/file") {
		return true
	}

	return false
}

func isFinished(raw string) bool {
	for _, s := range spinners {
		if strings.Contains(raw, s) {
			return false
		}
	}

	if strings.Contains(raw, "(esc to cancel") {
		return false
	}

	lower := strings.ToLower(raw)
	busyKeywords := []string{"searching", "initializing", "waiting for mcp", "Working...", "Queued"}
	for _, k := range busyKeywords {
		if strings.Contains(lower, strings.ToLower(k)) || strings.Contains(raw, k) {
			return false
		}
	}

	promptKeywords := []string{"Type your message", "? for shortcuts", "Ctrl+Y"}
	for _, p := range promptKeywords {
		if strings.Contains(raw, p) {
			return true
		}
	}

	return false
}

func (s *AgentSession) Write(input string, originTS string) error {
	s.mu.RLock()
	ready, waitChan := s.isReady, s.readyWait
	s.mu.RUnlock()

	if !ready {
		select {
		case <-waitChan:
		case <-time.After(90 * time.Second):
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.isProcessing = false
	s.originTS = originTS
	s.commandSentAt = time.Now()
	s.currentBuffer = ""
	s.sentOffset = 0
	s.currentMarker = fmt.Sprintf("===CMD_BOUNDARY_%d===", s.commandSentAt.UnixNano())

	_, replyTS, _ := s.api.PostMessage(s.channelID, slack.MsgOptionText("```\nJARVIS가 답변을 준비하고 있습니다...\n```", false), slack.MsgOptionTS(originTS))
	s.latestReplyTS = replyTS

	exec.Command("tmux", "send-keys", "-t", s.sessionName, "C-c", "Escape").Run()
	time.Sleep(100 * time.Millisecond)
	exec.Command("tmux", "send-keys", "-t", s.sessionName, "C-l").Run()
	time.Sleep(500 * time.Millisecond)

	exec.Command("tmux", "send-keys", "-t", s.sessionName, "C-u", fmt.Sprintf("printf '\\n\\n%s\\n\\n'", s.currentMarker), "C-m").Run()
	time.Sleep(400 * time.Millisecond)

	exec.Command("tmux", "send-keys", "-t", s.sessionName, "-l", html.UnescapeString(input)).Run()
	time.Sleep(200 * time.Millisecond)
	exec.Command("tmux", "send-keys", "-t", s.sessionName, "C-m").Run()

	s.isProcessing = true
	return nil
}

func (s *AgentSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	exec.Command("tmux", "kill-session", "-t", s.sessionName).Run()
	if s.ptyFile != nil {
		s.ptyFile.Close()
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*AgentSession
	api      *slack.Client
}

func (sm *SessionManager) GetOrCreate(channelID string) (*AgentSession, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if s, ok := sm.sessions[channelID]; ok {
		return s, nil
	}
	s, err := NewAgentSession(sm.api, channelID)
	if err == nil {
		sm.sessions[channelID] = s
	}
	return s, err
}

func main() {
	api := slack.New(os.Getenv("SLACK_BOT_TOKEN"), slack.OptionAppLevelToken(os.Getenv("SLACK_APP_TOKEN")))
	client := socketmode.New(api)
	sm := &SessionManager{sessions: make(map[string]*AgentSession), api: api}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		for evt := range client.Events {
			if evt.Type == socketmode.EventTypeEventsAPI {
				eventsAPIEvent, _ := evt.Data.(slackevents.EventsAPIEvent)
				client.Ack(*evt.Request)
				if ev, ok := eventsAPIEvent.InnerEvent.Data.(*slackevents.MessageEvent); ok {
					handleMessage(ev, sm, api)
				}
			}
		}
	}()
	log.Println("⚡ Gemini-Slack-Bot (Robust History Mode) 가동 중...")
	client.RunContext(ctx)
}

func handleMessage(ev *slackevents.MessageEvent, sm *SessionManager, api *slack.Client) {
	if ev.Channel != os.Getenv("ALLOWED_CHANNEL_ID") || ev.User == "" || ev.BotID != "" || !strings.HasPrefix(ev.Text, "!") {
		return
	}

	input := strings.TrimSpace(strings.TrimPrefix(ev.Text, "!"))
	if input == "reset" || input == "초기화" {
		sm.mu.Lock()
		if s, ok := sm.sessions[ev.Channel]; ok {
			s.Close()
			delete(sm.sessions, ev.Channel)
		}
		sm.mu.Unlock()
		api.PostMessage(ev.Channel, slack.MsgOptionText("🔄 세션이 완전히 초기화되었습니다. 다음 명령어 시 새로 시작합니다.", false), slack.MsgOptionTS(ev.TimeStamp))
		return
	}

	s, err := sm.GetOrCreate(ev.Channel)
	if err != nil {
		log.Printf("[Error] Session creation failed: %v", err)
		return
	}
	api.AddReaction("hourglass_flowing_sand", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
	if err := s.Write(input, ev.TimeStamp); err != nil {
		log.Printf("[Error] Write failed: %v", err)
	}
}
