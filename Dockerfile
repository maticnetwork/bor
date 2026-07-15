# ─── BUILDER STAGE ───────────────────────────────────────────────────────────────
FROM golang:1.26.5-alpine AS builder

ARG BOR_DIR=/var/lib/bor/
ARG GIT_COMMIT=""
ENV BOR_DIR=$BOR_DIR

RUN apk add --no-cache build-base git linux-headers

WORKDIR ${BOR_DIR}

COPY go.mod go.sum ./

RUN --mount=type=ssh \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=ssh \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ -f prod.pprof ]; then \
      PGO_FLAG="-pgo=prod.pprof"; \
    else \
      PGO_FLAG=""; \
    fi && \
    go build ${PGO_FLAG} -buildvcs=false \
      -ldflags "-X github.com/ethereum/go-ethereum/params.GitCommit=${GIT_COMMIT}" \
      -o ${BOR_DIR}/build/bin/bor ./cmd/cli/main.go

# ─── RUNTIME STAGE ────────────────────────────────────────────────────────────────
FROM alpine:3.23

ARG BOR_DIR=/var/lib/bor/
ENV BOR_DIR=$BOR_DIR

RUN apk add --no-cache bash ca-certificates wget && \
    mkdir -p ${BOR_DIR}

WORKDIR ${BOR_DIR}

COPY --from=builder ${BOR_DIR}/build/bin/bor /usr/bin/

EXPOSE 8545 8546 8547 30303 30303/udp 30304/udp

ENTRYPOINT ["bor"]
