FROM node:22-alpine AS frontend
WORKDIR /build/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS backend
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -o /aiwiki ./cmd/aiwiki

FROM alpine:3.22
WORKDIR /app
COPY --from=backend /aiwiki /app/aiwiki
COPY --from=frontend /build/web/dist /app/web/dist
COPY LICENSE THIRD_PARTY_NOTICES.md /app/
EXPOSE 8090
ENTRYPOINT ["/app/aiwiki"]
# Publish safely on the host: -p 127.0.0.1:8090:8090 (trusted mode has no authentication; configure OIDC for corporate mode).
CMD ["-addr", "0.0.0.0:8090", "-data", "/app/data", "-web", "/app/web/dist"]
