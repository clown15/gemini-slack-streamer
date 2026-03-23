# 🤖 Gemini Slack Bot (Robust History Mode)

이 프로젝트는 **Google Gemini CLI (Agent Mode)**를 Slack과 연동하여, 채팅창에서 강력한 AI 에이전트 기능을 실시간으로 사용할 수 있게 해주는 고성능 커넥터입니다. `tmux`와 `PTY` 기술을 결합하여 에이전트의 세션 상태를 완벽하게 유지하며, **Robust Sync 엔진**을 통해 터미널의 실시간 출력을 슬랙 스레드로 안전하게 스트리밍합니다.

---

## 🌟 주요 특장점 (Core Features)

### 1. 지속적 세션 및 상태 관리
- **Tmux 기반 세션 유지**: 봇 서버가 재시작되거나 네트워크가 끊겨도 에이전트의 작업 문맥(Context)과 히스토리를 `tmux` 세션 내에 안전하게 보존합니다.
- **채널별 독립 세션**: 각 슬랙 채널별로 전용 `tmux` 세션을 할당하여 사용자 간 작업 간섭을 원천 차단합니다.

### 2. 고신뢰성 동기화 엔진 (Robust Sync)
- **원자적 메시지 체이닝 (Atomic Chaining)**: 답변이 슬랙의 단일 메시지 제한(약 3,000자)을 초과할 경우, 데이터 유실 없이 자동으로 새로운 메시지를 생성하여 답변을 이어갑니다.
- **경계 격리 (Boundary Isolation)**: 각 명령어 실행 시마다 고유한 `CMD_BOUNDARY` 마커를 주입하여, 터미널 히스토리가 길어져도 현재 명령어에 대한 답변만을 정확히 추출하여 표시합니다.
- **실시간 적응형 업데이트 (Adaptive Sync)**: 터미널의 변화를 1.2초 간격으로 감지하여 슬랙 메시지를 실시간으로 업데이트(UpdateMessage)합니다.

### 3. 지능형 노이즈 필터링
- **터미널 노이즈 제거**: 스피너(⠋), 작업 시간 안내, 시스템 상태바, Gemini CLI 특유의 UI 요소(YOLO 모드 경고 등)를 본문에서 깔끔하게 제거하여 가독성을 극대화합니다.
- **유니코드 무결성**: 모든 텍스트 처리를 `rune` 단위로 수행하여 한글 등 멀티바이트 문자가 깨지는 현상을 완벽하게 방지합니다.

### 4. 사용자 경험 (UX) 최적화
- **상태 피드백**: 이모지 반응(⏳: 진행 중, ✅: 완료)을 통해 작업 상태를 직관적으로 제공합니다.
- **초기화 가드 (WaitReady)**: MCP 서버 로딩 등 초기화 과정을 자동 감지하여, 에이전트가 준비된 시점부터 입력을 처리합니다.

---

## 🛠 설치 및 실행 (Setup & Execution)

### 1. 요구 사항
- **Docker & Docker Compose**
- **Slack App**: Socket Mode 활성화 및 필요한 권한(Scopes) 설정
    - `app_mentions:read`, `chat:write`, `files:write`, `reactions:write` 등

### 2. 환경 설정 (`.env`)
`.env.example` 파일을 복사하여 `.env`를 생성하고 토큰을 입력합니다.

```env
SLACK_BOT_TOKEN=xoxb-your-bot-token
SLACK_APP_TOKEN=xapp-your-app-token
ALLOWED_CHANNEL_ID=C0123456789
GEMINI_BIN_PATH=gemini
```

### 3. 실행
```bash
# 컨테이너 빌드 및 백그라운드 실행
docker compose up -d --build
```

---

## ⌨️ 사용 방법 (Commands)

슬랙의 지정된 채널에서 `!` 접두사를 사용하여 명령어를 입력합니다.

- **`![명령어]`**: Gemini 에이전트에게 명령을 전달합니다. (예: `!현재 디렉토리의 파일 목록을 알려줘`)
- **`!reset`** 또는 **`!초기화`**: 현재 채널의 `tmux` 세션을 완전히 종료하고 히스토리를 초기화합니다.
- **자동 응답 업데이트**: 에이전트가 답변을 작성하는 동안 슬랙 메시지가 실시간으로 갱신됩니다.

---

## 🏗 시스템 아키텍처 (Architecture)

1. **Slack Socket Mode**: 실시간 이벤트 수신 및 메시지 처리 브릿지.
2. **Tmux Manager**: 에이전트의 PTY(Pseudo-Terminal)를 관리하고 세션 영속성 보장.
3. **Robust Sync Loop**: 
    - `capture-pane`을 통한 터미널 화면 캡처
    - ANSI 이스케이프 코드 제거 및 필터링
    - 슬랙 메시지 업데이트 및 초과 시 체이닝 처리.
4. **Emoji Feedback System**: 메시지 타임스탬프(TS)를 기준으로 상태 이모지 자동 제어.

---

## ⚠️ 주의사항
- **채널 제한**: 설정된 `ALLOWED_CHANNEL_ID`에서만 작동합니다.
- **비정상 종료 시**: `!reset` 명령어를 통해 세션을 다시 시작할 수 있습니다.
- **YOLO 모드**: 기본적으로 `--approval-mode=yolo` 옵션으로 실행되므로 에이전트의 동작을 신뢰할 수 있는 환경에서 사용하십시오.
