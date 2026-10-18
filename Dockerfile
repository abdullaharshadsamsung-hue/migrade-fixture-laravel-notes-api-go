FROM golang:1.25-alpine

WORKDIR /app

COPY . .

RUN go mod download && if [ -d ./cmd/server ]; then mkdir -p /app/bin && go build -o /app/bin/server ./cmd/server; else echo 'No ./cmd/server package present; skipping server build'; fi

EXPOSE 8080

CMD ["sh", "-c", "/app/bin/server"]
