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

var (
	// [추가] ANSI Escape Code 제거를 위한 정규식
	reANSI = regexp.MustCompile(`[\x1b\x9b][[()#;?]*(?:[0-9]{1,4}(?:;[0-9]{0,4})*)?[0-9A-ORZcf-nqry=><]`)
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
	threadTS       string
	originTS       string 
	currentBuffer  string
	currentMarker  string    // [추가] 현재 명령어의 시작을 알리는 유니크 마커
	sentOffset     int       // [추가] 이미 이전 메시지들에 확정되어 포함된 텍스트의 총 길이 (Rune 기준)
	commandSentAt  time.Time
	isProcessing   bool // 현재 명령어를 처리 중인지 여부
	isReady        bool
	readyWait      chan struct{}
	done           chan struct{}
}

func NewAgentSession(api *slack.Client, channelID string) (*AgentSession, error) {
	sessionName := fmt.Sprintf("gemini_%s", strings.ReplaceAll(channelID, "-", "_"))
	log.Printf("[Session] 고신뢰성 엔진 기동: %s", sessionName)

	exec.Command("tmux", "kill-session", "-t", sessionName).Run()
	geminiPath := os.Getenv("GEMINI_BIN_PATH")
	if geminiPath == "" { geminiPath = "gemini" }
	
	startCmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName, 
		fmt.Sprintf("LANG=C.UTF-8 LC_ALL=C.UTF-8 TERM=xterm-256color %s --approval-mode=yolo", geminiPath))
	if err := startCmd.Run(); err != nil { return nil, err }
	
	exec.Command("tmux", "set-option", "-t", sessionName, "status", "off").Run()
	exec.Command("tmux", "resize-window", "-t", sessionName, "-x", "100", "-y", "30").Run()

	attachCmd := exec.Command("tmux", "attach-session", "-t", sessionName)
	f, err := pty.Start(attachCmd)
	if err != nil { return nil, err }
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
			s.mu.Lock(); s.isReady = true; close(s.readyWait); s.mu.Unlock()
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
					s.mu.Lock()
					if !s.isReady { s.isReady = true; close(s.readyWait) }
					s.mu.Unlock()
					return
				}
			} else { stableCount = 0 }
		}
	}
}

func (s *AgentSession) syncLoop() {
	ticker := time.NewTicker(1200 * time.Millisecond)
	for {
		select {
		case <-s.done: return
		case <-ticker.C: s.adaptiveSync()
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

	// 답변 처리 중이 아니거나 메시지 TS가 없으면 건너뜀
	if s.latestReplyTS == "" || !s.isProcessing { return }

	// [개선] -S - 옵션으로 스크롤 히스토리 전체 캡처 및 ANSI 제거
	out, err := exec.Command("tmux", "capture-pane", "-pS", "-", "-t", s.sessionName).Output()
	if err != nil {
		log.Printf("[Sync] tmux capture error: %v", err)
		return
	}
	content := stripANSI(string(out))

	// [교정] 1. 현재 명령어의 시작점(Marker)을 먼저 찾음 (데이터 오염 방지)
	if s.currentMarker == "" { return }
	cmdStartIdx := strings.LastIndex(content, s.currentMarker)
	if cmdStartIdx == -1 { return } // 아직 마커가 출력되지 않음
	
	currentContent := content[cmdStartIdx:]

	// [교정] 2. 해당 명령어 이후의 첫 번째 답변 마커(✦) 탐색
	markerIdx := strings.Index(currentContent, "✦")
	if markerIdx == -1 { 
		// 아직 답변이 시작되지 않았으나, 종료 신호가 이미 있는지 확인 (프롬프트 복귀)
		if isFinished(currentContent) && time.Since(s.commandSentAt) > 10*time.Second {
			log.Printf("[Sync] 답변 없이 종료됨 (에러 가능성)")
			s.finalize()
		}
		return 
	}

	body := currentContent[markerIdx:]
	fullBody := cleanResponse(body)
	runes := []rune(fullBody)

	// 현재까지 전송된 총 길이 계산 (오프셋 + 현재 버퍼)
	totalSent := s.sentOffset + len([]rune(s.currentBuffer))

	// [수정] 모든 내용이 전송되었고 터미널이 종료 상태라면 마무리 처리
	if totalSent >= len(runes) && isFinished(currentContent) {
		// 마지막 전송 후 최소 8초 대기하여 안정성 확보 (생각 중인 상태 고려)
		if time.Since(s.commandSentAt) > 8*time.Second {
			log.Printf("[Sync] 명령어 처리 최종 완료 감지 (Total Rune: %d)", len(runes))
			s.finalize()
			return
		}
	}

	// 현재 윈도우(Offset)부터 나머지 전체를 가져옴
	remaining := runes[s.sentOffset:]
	
	// [고신뢰성 설계] 보수적 청크 크기(3000자) 및 에러 시 축소(2000자) 로직
	limit := 3000
	isOverLimit := len(remaining) > limit
	var chunk string
	
	// 최대 2회 시도 (기본 크기 -> 축소 크기)
	for retry := 0; retry < 2; retry++ {
		currentLimit := limit
		if retry > 0 { currentLimit = 2000 }
		
		if len(remaining) > currentLimit {
			chunk = string(remaining[:currentLimit])
			isOverLimit = true
		} else {
			chunk = string(remaining)
			isOverLimit = false
		}

		// 내용이 바뀌었을 때만 업데이트 수행 (단, 종료 감지를 위해 완전히 동일할 때만 리턴)
		if chunk == s.currentBuffer && !isFinished(content) {
			return
		}

		display := fmt.Sprintf("```\n%s\n```", chunk)
		if isOverLimit {
			display += "\n*(답변이 너무 길어 다음 메시지로 계속됩니다...)*"
		}

		// [1] 현재 메시지 업데이트
		_, _, _, err = s.api.UpdateMessage(s.channelID, s.latestReplyTS, slack.MsgOptionText(display, false))
		if err != nil {
			if strings.Contains(err.Error(), "msg_too_long") {
				log.Printf("[Warn] Message too long (%d runes). Retrying with smaller limit...", len(chunk))
				limit = 2000
				continue
			}
			log.Printf("[Sync] UpdateMessage error: %v", err)
			return
		}
		
		// 성공 시 버퍼 업데이트
		s.currentBuffer = chunk
		log.Printf("[Sync] Update SUCCESS | TS: %s | Runes: %d | isOverLimit: %v", s.latestReplyTS, len(chunk), isOverLimit)

		// [2] 임계치 도달 시 즉시 체이닝 처리 (루프 내에서 처리하여 원자성 확보)
		if isOverLimit {
			actualSent := len([]rune(s.currentBuffer))
			log.Printf("[Chain] 임계치 도달 -> 다음 메시지 생성 시도 (현재 전송분: %d)", actualSent)
			
			// [안전] 새 메시지가 준비될 때까지 잠시 업데이트 중단
			s.latestReplyTS = "" 

			_, nextTS, err := s.api.PostMessage(s.channelID, 
				slack.MsgOptionText("```\n(답변 계속됨...)\n```", false), 
				slack.MsgOptionTS(s.originTS))
			
			if err == nil {
				// 성공 시에만 오프셋을 옮기고 TS를 교체
				s.sentOffset += actualSent
				s.latestReplyTS = nextTS
				s.currentBuffer = "" // 새 메시지는 빈 상태로 시작
				log.Printf("[Chain] Chaining SUCCESS | New TS: %s | New Offset: %d", nextTS, s.sentOffset)
			} else {
				log.Printf("[Chain] PostMessage error: %v. Will retry next tick.", err)
				// 실패 시 Offset을 옮기지 않으므로 다음 루프에서 현재 메시지에 다시 UpdateMessage를 시도하게 됨
				// 단, TS를 복구해야 함 (현재 루프에서는 break 하므로 다음 루프에서 재시도)
			}
		}
		break
	}
}

func cleanResponse(raw string) string {
	lines := strings.Split(raw, "\n")
	var result []string
	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "⠋", "⠓", "⠒", "⠚", "⠝"}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		
		// [개선] 푸터, 상태바, 스피너 및 작업 안내 정보 제거
		skip := false
		for _, s := range spinners {
			if strings.Contains(line, s) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		if strings.Contains(line, "▀▀▀▀") || strings.Contains(line, "▄▄▄▄") || 
		   strings.Contains(line, "────────") || // 구분선 (다양한 길이 대응)
		   strings.Contains(line, "YOLO") || // YOLO 모드
		   strings.Contains(line, "Ctrl+Y") || // 단축키 안내
		   strings.Contains(line, "MCP servers") || // 서버 정보
		   strings.Contains(line, "GEMINI.md file") || // 파일 정보
		   strings.Contains(line, "workspace (/") || 
		   strings.Contains(line, "Auto (Gemini") ||
		   strings.Contains(line, "CMD_BOUNDARY") || // [추가] 유니크 마커 필터링
		   strings.Contains(line, "(esc to cancel") || // 작업 시간 정보
		   strings.Contains(line, "Type your message") { // 프롬프트 텍스트
			continue 
		}
		
		// 실제 프롬프트 입력줄 및 특수 기호 줄 감지
		if (strings.HasPrefix(trimmed, ">") && len(trimmed) < 4) || 
		   strings.HasPrefix(trimmed, "*   Type your message") ||
		   strings.HasPrefix(trimmed, "Type your message or @path/to/file") {
			continue
		}

		result = append(result, strings.TrimRight(line, " \r\t"))
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

func isFinished(raw string) bool {
	// [핵심] 진행 중임을 나타내는 강력한 신호들 (스피너 및 취소 안내)
	// 이 기호들이나 문구가 포함되어 있다면 하단에 프롬프트가 보여도 아직 작업 중임.
	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "⠋", "⠓", "⠒", "⠚", "⠝"}
	for _, s := range spinners {
		if strings.Contains(raw, s) {
			return false
		}
	}

	if strings.Contains(raw, "(esc to cancel") {
		return false
	}

	lower := strings.ToLower(raw)
	// 현재 작업 중임을 나타내는 키워드들
	isBusy := strings.Contains(lower, "searching") || 
	          strings.Contains(lower, "initializing") || 
	          strings.Contains(lower, "waiting for mcp") ||
	          strings.Contains(raw, "Working...") ||
	          strings.Contains(raw, "Queued")

	if isBusy {
		return false
	}

	// [종료 조건] 진행 중 신호가 없고, 프롬프트나 도움말 안내가 보일 때
	hasPrompt := strings.Contains(raw, "? for shortcuts")

	return hasPrompt
}

func (s *AgentSession) Write(input string, originTS string) error {
	s.mu.RLock()
	isReady, readyWait := s.isReady, s.readyWait
	s.mu.RUnlock()

	if !isReady {
		select {
		case <-readyWait:
		case <-time.After(90 * time.Second):
		}
	}

	s.mu.Lock()
	s.originTS = originTS
	s.commandSentAt = time.Now()
	s.currentBuffer = ""
	s.sentOffset = 0      
	s.isProcessing = true 
	
	// [추가] 유니크한 명령어 경계 마커 생성
	s.currentMarker = fmt.Sprintf("===CMD_BOUNDARY_%d===", time.Now().UnixNano())
	
	// [중요] 새로운 응답 메시지 생성 (이력 보존)
	_, replyTS, _ := s.api.PostMessage(s.channelID, slack.MsgOptionText("```\nJARVIS가 답변을 준비하고 있습니다...\n```", false), slack.MsgOptionTS(originTS))
	s.latestReplyTS = replyTS
	
	// 첫 메시지면 10초, 이후 3초 (에이전트 준비 시간)
	wait := 3 * time.Second
	if strings.Contains(input, "reset") { wait = 1 * time.Second } // 초기화는 빠르게
	time.Sleep(wait)
	s.mu.Unlock()

	// 화면 시각적 정리 및 명령어 입력
	exec.Command("tmux", "send-keys", "-t", s.sessionName, "C-c", "Escape").Run()
	time.Sleep(100 * time.Millisecond)
	exec.Command("tmux", "send-keys", "-t", s.sessionName, "C-l").Run() // 시각적 화면 정리
	// [맥락 보존] tmux clear-history는 호출하지 않음 (이전 대화 맥락 유지)
	time.Sleep(500 * time.Millisecond)
	
	// [교정] 명령어 시작 마커를 확실히 출력하여 파싱의 기준점으로 삼음 (printf 사용)
	exec.Command("tmux", "send-keys", "-t", s.sessionName, "C-u", fmt.Sprintf("printf '\\n\\n%s\\n\\n'", s.currentMarker), "C-m").Run()
	time.Sleep(500 * time.Millisecond)

	exec.Command("tmux", "send-keys", "-t", s.sessionName, "C-u").Run()
	exec.Command("tmux", "send-keys", "-t", s.sessionName, "-l", html.UnescapeString(input)).Run()
	time.Sleep(200 * time.Millisecond)
	exec.Command("tmux", "send-keys", "-t", s.sessionName, "C-m").Run()
	return nil
}

func (s *AgentSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	exec.Command("tmux", "kill-session", "-t", s.sessionName).Run()
	if s.ptyFile != nil { s.ptyFile.Close() }
	select {
	case <-s.done:
	default: close(s.done)
	}
}

type SessionManager struct {
	mu sync.RWMutex
	sessions map[string]*AgentSession
	api *slack.Client
}

func (sm *SessionManager) GetOrCreate(channelID string) (*AgentSession, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if s, ok := sm.sessions[channelID]; ok { return s, nil }
	s, err := NewAgentSession(sm.api, channelID)
	if err == nil { sm.sessions[channelID] = s }
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
					if ev.Channel == os.Getenv("ALLOWED_CHANNEL_ID") && ev.User != "" && ev.BotID == "" && strings.HasPrefix(ev.Text, "!") {
						input := strings.TrimSpace(strings.TrimPrefix(ev.Text, "!"))
						if input == "reset" || input == "초기화" {
							sm.mu.Lock()
							if s, ok := sm.sessions[ev.Channel]; ok {
								s.mu.Lock()
								exec.Command("tmux", "kill-session", "-t", s.sessionName).Run()
								s.mu.Unlock()
								delete(sm.sessions, ev.Channel)
							}
							sm.mu.Unlock()
							api.PostMessage(ev.Channel, slack.MsgOptionText("🔄 세션이 완전히 초기화되었습니다. 다음 명령어 시 새로 시작합니다.", false), slack.MsgOptionTS(ev.TimeStamp))
						} else {
							s, _ := sm.GetOrCreate(ev.Channel)
							api.AddReaction("hourglass_flowing_sand", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
							s.Write(input, ev.TimeStamp)
						}
					}
				}
			}
		}
	}()
	log.Println("⚡ Gemini-Slack-Bot (Robust History Mode) 가동 중...")
	client.RunContext(ctx)
}
