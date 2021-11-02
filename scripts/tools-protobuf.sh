curl -LO https://github.com/protocolbuffers/protobuf/releases/download/v3.12.0/protoc-3.12.0-linux-x86_64.zip
unzip protoc-3.12.0-linux-x86_64.zip -d $HOME/.local
export PATH="$PATH:$HOME/.local/bin"

go get google.golang.org/grpc

sudo apt-get install -y golang-protobuf-extensions-dev
