# Ref (with correction): https://github.com/maticnetwork/bor/blob/develop/docs/config.md

chain = "mainnet"
log-level = "info"
datadir = ""
syncmode = "fast"
gcmode = "full"
snapshot = true
ethstats = ""
requiredblocks = {}

p2p {
    maxpeers = 30
    maxpendpeers = 50
    bind = "0.0.0.0"
    port = 30303
    nodiscover = false
    nat = "any"
    discovery {
        v5disc = false
        bootnodes = []
        bootnodesv4 = []
        bootnodesv5 = []
        static-nodes = []
        trusted-nodes = []
        dns = []
    }
}

heimdall {
    url = "http://localhost:1317"
    without = false
}

txpool {
    locals = []
    nolocals = false
    journal = ""
    rejournal = "1h"
    pricelimit = 1
    pricebump = 10
    accountslots = 16
    globalslots = 4096
    accountqueue = 64
    globalqueue = 1024
    lifetime = "3h"
}

miner {
    mine = false
    etherbase = ""
    gasceil = 8000000
    extradata = ""
}

gpo {
    blocks = 20
    percentile = 60
}

jsonrpc {
    ipc-disable = false
    ipc-path = ""
    
    http {
        enabled = false
        port = 8545
        prefix = ""
        host = "localhost"
		api = ["web3", "net"]
        corsdomain = ["*"]
        vhosts = ["*"]
    }

    ws {
        enabled = false
        port = 8546
        prefix = ""
        host = "localhost"
		api = ["web3", "net"]
        corsdomain = ["*"]
        vhosts = ["*"]
    }

    graphql {
        enabled = false
		api = ["web3", "net"]
        corsdomain = ["*"]
        vhosts = ["*"]
    }
}

telemetry {
    metrics = false
    expensive = false

    influx {
        influxdb = false
        endpoint = ""
        database = ""
        username = ""
        password = ""
        influxdbv2 = false
        token = ""
        bucket = ""
        organization = ""
    }
}

cache {
    cache = 1024
    database = 50
    trie = 15
    gc = 25
    snapshot = 10
    journal = "triecache"
    rejournal = "60m"
    noprefetch = false
    preimages = false
    txlookuplimit = 2350000
}

accounts {
    unlock = []
    passwordfile = ""
    allow-insecure-unlock = false
    lightkdf = false
}

grpc {
    addr = ":3131"
}

developer {
	dev = true
	period = 2
}