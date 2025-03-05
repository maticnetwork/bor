FROM golang:latest

ARG BOR_DIR=/var/lib/bor
ENV BOR_DIR=$BOR_DIR

RUN apt-get update -y && apt-get upgrade -y \
  && apt install build-essential git -y \
  && mkdir -p ${BOR_DIR}

WORKDIR ${BOR_DIR}

# Copy go.mod and go.sum first to leverage Docker's cache
COPY go.mod go.sum ./

# Configure git for SSH and install dependencies
RUN --mount=type=ssh git config --global url."git@github.com:".insteadOf "https://github.com/" \
  && mkdir -p ~/.ssh \
  && ssh-keyscan github.com >> ~/.ssh/known_hosts

# Download dependencies separately (this layer can be cached)
RUN --mount=type=ssh go mod download

# Copy the rest of the application code
COPY . .

# Build the application
RUN --mount=type=ssh make bor

RUN cp build/bin/bor /usr/bin/

ENV SHELL /bin/bash
EXPOSE 8545 8546 8547 30303 30303/udp

ENTRYPOINT ["bor"]