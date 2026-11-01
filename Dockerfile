# Сборка в два этапа: компиляция на платформе сборки, итоговый образ - один
# статический файл в distroless без оболочки, от пользователя nonroot.
# Сборка под arm64: docker buildx build --platform linux/arm64 -t hcgateway .

FROM --platform=$BUILDPLATFORM golang:1.26.4 AS build
ARG TARGETOS TARGETARCH
# VERSION - версия шлюза в журнале и в метрике hcgateway_build_info: --build-arg VERSION=...
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/hcgateway ./cmd/hcgateway
# Пустой каталог для тома: Docker создаёт том с владельцем из образа, иначе - root.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hcgateway /hcgateway
COPY --from=build --chown=nonroot:nonroot /out/data /data
# /config - каталог настроек: веб-интерфейс сохраняет в него config.yaml и config.yaml.bak.
# /data - ключи приборов, токены Яндекса и пользователь веб-интерфейса.
VOLUME ["/data"]
# 8080 - запросы Яндекса и веб-интерфейс под /ui, только через Traefik.
# 8082 - метрики для Prometheus в сети Docker.
EXPOSE 8080 8082
USER nonroot:nonroot
ENTRYPOINT ["/hcgateway", "-config", "/config/config.yaml"]
CMD ["serve"]
