module github.com/ethereum/go-ethereum

go 1.24.4

require (
	github.com/0xPolygon/heimdall-v2 v0.2.8-0.20250708101522-d403fe733391
	github.com/Azure/azure-sdk-for-go/sdk/storage/azblob v1.3.2
	github.com/BurntSushi/toml v1.4.0
	github.com/JekaMas/go-grpc-net-conn v0.0.0-20220708155319-6aff21f2d13d
	github.com/JekaMas/workerpool v1.1.8
	github.com/Microsoft/go-winio v0.6.2
	github.com/VictoriaMetrics/fastcache v1.12.2
	github.com/aws/aws-sdk-go-v2 v1.27.2
	github.com/aws/aws-sdk-go-v2/config v1.27.18
	github.com/aws/aws-sdk-go-v2/credentials v1.17.18
	github.com/aws/aws-sdk-go-v2/service/route53 v1.40.10
	github.com/cespare/cp v1.1.1
	github.com/cloudflare/cloudflare-go v0.97.0
	github.com/cockroachdb/pebble v1.1.2
	github.com/consensys/gnark-crypto v0.16.0
	github.com/cosmos/cosmos-sdk v0.50.13
	github.com/cosmos/gogoproto v1.7.0
	github.com/crate-crypto/go-eth-kzg v1.3.0
	github.com/crate-crypto/go-ipa v0.0.0-20240724233137-53bbb0ceb27a
	github.com/crate-crypto/go-kzg-4844 v1.1.0
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc
	github.com/deckarep/golang-set/v2 v2.6.0
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.3.0
	github.com/donovanhide/eventsource v0.0.0-20210830082556-c59027999da0
	github.com/dop251/goja v0.0.0-20240516125602-ccbae20bcec2
	github.com/emirpasic/gods v1.18.1
	github.com/ethereum/c-kzg-4844/v2 v2.1.0
	github.com/ethereum/go-verkle v0.2.2
	github.com/fatih/color v1.17.0
	github.com/ferranbt/fastssz v0.1.2
	github.com/fjl/gencodec v0.1.0
	github.com/fsnotify/fsnotify v1.7.0
	github.com/gballet/go-libpcsclite v0.0.0-20191108122812-4678299bea08
	github.com/gofrs/flock v0.12.1
	github.com/golang-jwt/jwt/v4 v4.5.2
	github.com/golang/mock v1.6.0
	github.com/golang/snappy v0.0.5-0.20220116011046-fa5810519dcb
	github.com/google/gofuzz v1.2.0
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/graph-gophers/graphql-go v1.5.0
	github.com/grpc-ecosystem/go-grpc-middleware v1.4.0
	github.com/hashicorp/go-bexpr v0.1.14
	github.com/hashicorp/golang-lru v1.0.2
	github.com/hashicorp/hcl/v2 v2.20.1
	github.com/heimdalr/dag v1.4.0
	github.com/holiman/billy v0.0.0-20240322075458-72a4e81ec6da
	github.com/holiman/bloomfilter/v2 v2.0.3
	github.com/holiman/uint256 v1.3.2
	github.com/huin/goupnp v1.3.0
	github.com/imdario/mergo v0.3.13
	github.com/influxdata/influxdb-client-go/v2 v2.13.0
	github.com/influxdata/influxdb1-client v0.0.0-20220302092344-a9ab5670611c
	github.com/jackpal/go-nat-pmp v1.0.2
	github.com/jedisct1/go-minisign v0.0.0-20230811132847-661be99b8267
	github.com/json-iterator/go v1.1.12
	github.com/karalabe/hid v1.0.1-0.20240306101548-573246063e52
	github.com/kylelemons/godebug v1.1.0
	github.com/maticnetwork/crand v1.0.2
	github.com/maticnetwork/polyproto v0.0.6
	github.com/mattn/go-colorable v0.1.13
	github.com/mattn/go-isatty v0.0.20
	github.com/mitchellh/cli v1.1.5
	github.com/mitchellh/go-homedir v1.1.0
	github.com/naoina/toml v0.1.2-0.20170918210437-9fafd6967416
	github.com/olekukonko/tablewriter v0.0.5
	github.com/pelletier/go-toml v1.9.5
	github.com/peterh/liner v1.2.2
	github.com/pion/stun/v2 v2.0.0
	github.com/protolambda/bls12-381-util v0.1.0
	github.com/protolambda/zrnt v0.34.1
	github.com/protolambda/ztyp v0.2.2
	github.com/rs/cors v1.11.1
	github.com/ryanuber/columnize v2.1.2+incompatible
	github.com/shirou/gopsutil v3.21.11+incompatible
	github.com/status-im/keycard-go v0.3.2
	github.com/stretchr/testify v1.10.0
	github.com/supranational/blst v0.3.14
	github.com/syndtr/goleveldb v1.0.1-0.20220721030215-126854af5e6d
	github.com/urfave/cli/v2 v2.27.5
	github.com/xsleonard/go-merkle v1.1.0
	go.opentelemetry.io/otel v1.34.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.27.0
	go.opentelemetry.io/otel/sdk v1.32.0
	go.uber.org/automaxprocs v1.6.0
	go.uber.org/goleak v1.3.0
	golang.org/x/crypto v0.36.0
	golang.org/x/exp v0.0.0-20250106191152-7588d65b2ba8
	golang.org/x/sync v0.12.0
	golang.org/x/sys v0.31.0
	golang.org/x/text v0.23.0
	golang.org/x/time v0.9.0
	golang.org/x/tools v0.29.0
	google.golang.org/grpc v1.70.0
	google.golang.org/protobuf v1.36.5
	gopkg.in/natefinch/lumberjack.v2 v2.2.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cloud.google.com/go/iam v1.1.6 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.2 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/pelletier/go-toml/v2 v2.2.2 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	gopkg.in/ini.v1 v1.67.0 // indirect
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.11.1 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.5.2 // indirect
	github.com/DataDog/zstd v1.5.5 // indirect
	github.com/agext/levenshtein v1.2.1 // indirect
	github.com/apparentlymart/go-textseg/v13 v13.0.0 // indirect
	github.com/armon/go-radix v1.0.0 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.16.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.11.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.20.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.28.12 // indirect
	github.com/aws/smithy-go v1.20.2 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bits-and-blooms/bitset v1.22.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cockroachdb/errors v1.11.3 // indirect
	github.com/cockroachdb/fifo v0.0.0-20240606204812-0bbfbd93a7ce // indirect
	github.com/cockroachdb/logtags v0.0.0-20230118201751-21c54148d20b // indirect
	github.com/cockroachdb/redact v1.1.5 // indirect
	github.com/cockroachdb/tokenbucket v0.0.0-20230807174530-cc333fc44b06 // indirect
	github.com/consensys/bavard v0.1.27 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.6 // indirect
	github.com/dlclark/regexp2 v1.7.0 // indirect
	github.com/garslo/gogen v0.0.0-20170306192744-1d203ffc1f61 // indirect
	github.com/getsentry/sentry-go v0.27.0 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/goccy/go-json v0.10.4 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/google/go-querystring v1.1.0 // indirect
	github.com/google/pprof v0.0.0-20241210010833-40e02aabc2ad // indirect
	github.com/influxdata/line-protocol v0.0.0-20210311194329-9aa0e372d097 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/kilic/bls12-381 v0.1.0 // indirect
	github.com/klauspost/compress v1.17.11 // indirect
	github.com/klauspost/cpuid/v2 v2.2.5 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-runewidth v0.0.13 // indirect
	github.com/minio/sha256-simd v1.0.0 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/mitchellh/pointerstructure v1.2.1 // indirect
	github.com/mmcloughlin/addchain v0.4.0 // indirect
	github.com/naoina/go-stringutil v0.1.0 // indirect
	github.com/opentracing/opentracing-go v1.2.0 // indirect
	github.com/pion/dtls/v2 v2.2.7 // indirect
	github.com/pion/logging v0.2.2 // indirect
	github.com/pion/transport/v2 v2.2.1 // indirect
	github.com/pion/transport/v3 v3.0.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_golang v1.20.5 // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.62.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/rivo/uniseg v0.2.0 // indirect
	github.com/rogpeppe/go-internal v1.13.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/tklauser/go-sysconf v0.3.12 // indirect
	github.com/tklauser/numcpus v0.6.1 // indirect
	github.com/xrash/smetrics v0.0.0-20240521201337-686a1a2994c1 // indirect
	golang.org/x/mod v0.22.0 // indirect
	golang.org/x/net v0.38.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gotest.tools v2.2.0+incompatible
	pgregory.net/rapid v1.1.0
	rsc.io/tmplfunc v0.0.3 // indirect
)

require (
	cloud.google.com/go v0.112.1 // indirect
	cloud.google.com/go/compute/metadata v0.5.2 // indirect
	cloud.google.com/go/pubsub v1.36.1 // indirect
	cosmossdk.io/api v0.7.5 // indirect
	cosmossdk.io/client/v2 v2.0.0-beta.6 // indirect
	cosmossdk.io/collections v0.4.0 // indirect
	cosmossdk.io/core v0.11.1 // indirect
	cosmossdk.io/depinject v1.0.0 // indirect
	cosmossdk.io/errors v1.0.1 // indirect
	cosmossdk.io/log v1.4.1 // indirect
	cosmossdk.io/math v1.4.0 // indirect
	cosmossdk.io/store v1.1.1 // indirect
	cosmossdk.io/tools/confix v0.1.1 // indirect
	cosmossdk.io/x/tx v0.13.7 // indirect
	filippo.io/edwards25519 v1.0.0 // indirect
	github.com/99designs/go-keychain v0.0.0-20191008050251-8e49817e8af4 // indirect
	github.com/99designs/keyring v1.2.1 // indirect
	github.com/DataDog/datadog-go v3.2.0+incompatible // indirect
	github.com/Masterminds/semver/v3 v3.3.1 // indirect
	github.com/Masterminds/sprig/v3 v3.2.1 // indirect
	github.com/RichardKnop/logging v0.0.0-20190827224416-1a693bdd4fae // indirect
	github.com/RichardKnop/machinery v1.10.8 // indirect
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/aws/aws-sdk-go v1.44.274 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.3.9 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.6.9 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.11.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.24.5 // indirect
	github.com/bgentry/speakeasy v0.1.1-0.20220910012023-760eaf8b6816 // indirect
	github.com/bradfitz/gomemcache v0.0.0-20190913173617-a41fca850d0b // indirect
	github.com/btcsuite/btcd/btcec/v2 v2.3.4 // indirect
	github.com/cbergoon/merkletree v0.2.0 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/chzyer/readline v1.5.1 // indirect
	github.com/cockroachdb/apd/v2 v2.0.2 // indirect
	github.com/cometbft/cometbft v0.38.17 // indirect
	github.com/cometbft/cometbft-db v0.14.1 // indirect
	github.com/cosmos/btcutil v1.0.5 // indirect
	github.com/cosmos/cosmos-db v1.0.2 // indirect
	github.com/cosmos/cosmos-proto v1.0.0-beta.5 // indirect
	github.com/cosmos/go-bip39 v1.0.0 // indirect
	github.com/cosmos/gogogateway v1.2.0 // indirect
	github.com/cosmos/iavl v1.2.0 // indirect
	github.com/cosmos/ics23/go v0.11.0 // indirect
	github.com/cosmos/ledger-cosmos-go v0.13.3 // indirect
	github.com/creachadair/atomicfile v0.3.1 // indirect
	github.com/creachadair/tomledit v0.0.24 // indirect
	github.com/danieljoos/wincred v1.2.1 // indirect
	github.com/desertbit/timer v0.0.0-20180107155436-c41aec40b27f // indirect
	github.com/dgraph-io/badger/v4 v4.2.0 // indirect
	github.com/dgraph-io/ristretto v0.1.1 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/dvsekhvalnov/jose2go v1.7.0 // indirect
	github.com/emicklei/dot v1.6.1 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/gammazero/deque v0.2.1 // indirect
	github.com/go-kit/kit v0.13.0 // indirect
	github.com/go-kit/log v0.2.1 // indirect
	github.com/go-logfmt/logfmt v0.6.0 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-redis/redis/v8 v8.11.5 // indirect
	github.com/go-redsync/redsync/v4 v4.8.1 // indirect
	github.com/godbus/dbus v0.0.0-20190726142602-4481cbc300e2 // indirect
	github.com/gogo/googleapis v1.4.1 // indirect
	github.com/golang/glog v1.2.4 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/gomodule/redigo v2.0.0+incompatible // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/flatbuffers v2.0.8+incompatible // indirect
	github.com/google/go-cmp v0.6.0 // indirect
	github.com/google/orderedcode v0.0.1 // indirect
	github.com/google/s2a-go v0.1.7 // indirect
	github.com/googleapis/gax-go/v2 v2.12.3 // indirect
	github.com/gorilla/handlers v1.5.2 // indirect
	github.com/gorilla/mux v1.8.1 // indirect
	github.com/grpc-ecosystem/grpc-gateway v1.16.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.22.0 // indirect
	github.com/gsterjov/go-libsecret v0.0.0-20161001094733-a6f4afe4910c // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-hclog v1.6.3 // indirect
	github.com/hashicorp/go-immutable-radix v1.3.1 // indirect
	github.com/hashicorp/go-metrics v0.5.3 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-plugin v1.5.2 // indirect
	github.com/hashicorp/go-retryablehttp v0.7.7 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/hashicorp/hcl v1.0.0 // indirect
	github.com/hashicorp/yamux v0.1.1 // indirect
	github.com/hdevalence/ed25519consensus v0.1.0 // indirect
	github.com/huandu/skiplist v1.2.0 // indirect
	github.com/huandu/xstrings v1.3.2 // indirect
	github.com/iancoleman/strcase v0.3.0 // indirect
	github.com/improbable-eng/grpc-web v0.15.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jmhodges/levigo v1.0.0 // indirect
	github.com/kelseyhightower/envconfig v1.4.0 // indirect
	github.com/lib/pq v1.10.9 // indirect
	github.com/linxGnu/grocksdb v1.8.14 // indirect
	github.com/magiconair/properties v1.8.7 // indirect
	github.com/manifoldco/promptui v0.9.0 // indirect
	github.com/minio/highwayhash v1.0.3 // indirect
	github.com/mitchellh/copystructure v1.0.0 // indirect
	github.com/mitchellh/go-testing-interface v1.14.1 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/mitchellh/reflectwalk v1.0.1 // indirect
	github.com/montanaflynn/stats v0.7.0 // indirect
	github.com/mtibben/percent v0.2.1 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oapi-codegen/runtime v1.0.0 // indirect
	github.com/oasisprotocol/curve25519-voi v0.0.0-20230904125328-1f23a7beb09a // indirect
	github.com/oklog/run v1.1.0 // indirect
	github.com/petermattis/goid v0.0.0-20240813172612-4fcff4a6cae7 // indirect
	github.com/posener/complete v1.2.3 // indirect
	github.com/rabbitmq/amqp091-go v1.10.0 // indirect
	github.com/rcrowley/go-metrics v0.0.0-20201227073835-cf1acfcdf475 // indirect
	github.com/redis/go-redis/v9 v9.7.3 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/rs/zerolog v1.33.0 // indirect
	github.com/sagikazarmark/locafero v0.4.0 // indirect
	github.com/sagikazarmark/slog-shim v0.1.0 // indirect
	github.com/sasha-s/go-deadlock v0.3.5 // indirect
	github.com/shopspring/decimal v1.2.0 // indirect
	github.com/sourcegraph/conc v0.3.0 // indirect
	github.com/spf13/afero v1.11.0 // indirect
	github.com/spf13/cast v1.6.0 // indirect
	github.com/spf13/cobra v1.8.1 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/spf13/viper v1.19.0 // indirect
	github.com/streadway/amqp v1.1.0 // indirect
	github.com/tendermint/go-amino v0.16.0 // indirect
	github.com/tidwall/btree v1.7.0 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.1.2 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20181117223130-1be2e3e5546d // indirect
	github.com/yusufpapurcu/wmi v1.2.3 // indirect
	github.com/zondax/ledger-go v0.14.3 // indirect
	go.etcd.io/bbolt v1.4.0-alpha.0.0.20240404170359-43604f3112c5 // indirect
	go.mongodb.org/mongo-driver v1.14.0 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.49.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.58.0 // indirect
	go.opentelemetry.io/otel/metric v1.34.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/oauth2 v0.29.0 // indirect
	golang.org/x/term v0.30.0 // indirect
	google.golang.org/api v0.171.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20250207221924-e9438ea467c6 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250124145028-65684f501c47 // indirect
	gotest.tools/v3 v3.5.1 // indirect
	nhooyr.io/websocket v1.8.7 // indirect
	sigs.k8s.io/yaml v1.4.0 // indirect
)

require (
	github.com/Masterminds/goutils v1.1.1 // indirect
	github.com/prometheus/tsdb v0.10.0
	github.com/zclconf/go-cty v1.13.0 // indirect
	github.com/zondax/hid v0.9.2 // indirect
	go.opencensus.io v0.24.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.27.0 // indirect
	go.opentelemetry.io/otel/trace v1.34.0
	go.opentelemetry.io/proto/otlp v1.3.1 // indirect
	google.golang.org/genproto v0.0.0-20240227224415-6ceb2ff114de // indirect
)

replace (
	cosmossdk.io/client/v2 => github.com/0xPolygon/cosmos-sdk/client/v2 v2.0.0-beta.1
	github.com/Masterminds/goutils => github.com/Masterminds/goutils v1.1.1
	github.com/cometbft/cometbft => github.com/0xPolygon/cometbft v0.2.0-polygon
	github.com/cosmos/cosmos-sdk => github.com/0xPolygon/cosmos-sdk v0.2.0-polygon
	github.com/ethereum/go-ethereum => github.com/maticnetwork/bor v1.14.14-0.20250618164709-3dc7d31cdac3
	github.com/ethereum/go-ethereum/common/math => github.com/maticnetwork/bor/common/math v1.5.5
	github.com/tendermint/tendermint => github.com/maticnetwork/tendermint v0.33.0
	github.com/tendermint/tm-db => github.com/tendermint/tm-db v0.2.0
	go.mongodb.org/mongo-driver => go.mongodb.org/mongo-driver v1.14.0
)
