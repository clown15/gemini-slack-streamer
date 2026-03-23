# 1. Go 1.26.1 공식 이미지 (Debian Bookworm 기반)
FROM golang:1.26.1-bookworm

# 2. 필수 OS 패키지 및 최신 안정화 Node.js (v22) 설치 (변하지 않는 레이어)
RUN apt-get update && apt-get install -y curl tmux && \
    curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && \
    apt-get install -y nodejs && \
    rm -rf /var/lib/apt/lists/*

# 3. Gemini CLI 설치 (Node 22 환경에서 완벽 호환)
RUN npm install -g @google/gemini-cli@latest

# 4. 사용자 생성 (동적 ID 할당)
ARG USER_NAME=ubuntu
ARG USER_ID=1000
ARG GROUP_ID=1000

RUN groupadd -g ${GROUP_ID} ${USER_NAME} || true && \
    useradd -u ${USER_ID} -g ${GROUP_ID} -m -s /bin/bash ${USER_NAME} || true
RUN mkdir -p /app && chown -R ${USER_ID}:${GROUP_ID} /app
USER ${USER_NAME}

# 5. 앱 빌드 (의존성 캐싱 활용)
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN go build -o slack-bot main.go && chmod +x /app/slack-bot

# 6. 작업 공간 설정 (여기서부터 WORKSPACE_PATH 변경 시 캐시 깨짐)
ARG WORKSPACE_PATH=/app/workspace
RUN mkdir -p ${WORKSPACE_PATH}

# 7. 실행
CMD ["/app/slack-bot"]
