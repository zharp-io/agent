FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
COPY zharp-collector-linux-${TARGETARCH} /zharp-collector

# 4317/4318 OTLP, 13133 health, 55679 zpages. Sidecar mode adds 24224 Fluent Forward
# (ECS FireLens) and 8125/udp StatsD.
EXPOSE 4317 4318 13133 55679 24224 8125/udp

# Host/daemon mode reads a mounted config. Sidecar mode (ZHARP_MODE=sidecar, or the
# `sidecar` argument) ignores this CMD and builds its config from the environment.
# There is no shell in this image: container health checks run `/zharp-collector health`.
ENTRYPOINT ["/zharp-collector"]
CMD ["--config", "/etc/zharp-collector/config.yaml"]
