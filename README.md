# Bor Overview
Bor is the Official Golang implementation of the Polygon PoS blockchain. It is a fork of [geth](https://github.com/ethereum/go-ethereum) and is EVM compatible (upto London fork).

![Forks](https://img.shields.io/github/forks/maticnetwork/bor?style=social)
![Stars](https://img.shields.io/github/stars/maticnetwork/bor?style=social)
![Languages](https://img.shields.io/github/languages/count/maticnetwork/bor)
![Issues](https://img.shields.io/github/issues/maticnetwork/bor)
![PRs](https://img.shields.io/github/issues-pr-raw/maticnetwork/bor)
![MIT License](https://img.shields.io/github/license/maticnetwork/bor)
![contributors](https://img.shields.io/github/contributors-anon/maticnetwork/bor)
![size](https://img.shields.io/github/languages/code-size/maticnetwork/bor)
![lines](https://img.shields.io/tokei/lines/github/maticnetwork/bor)
[![Discord](https://img.shields.io/discord/714888181740339261?color=1C1CE1&label=Polygon%20%7C%20Discord%20%F0%9F%91%8B%20&style=flat-square)](https://discord.gg/zdwkdvMNY2)
[![Twitter Follow](https://img.shields.io/twitter/follow/0xPolygon.svg?style=social)](https://twitter.com/0xPolygon)

### Installing bor using packaging

The easiest way to get started with bor is to install the packages using the command below. Refer to the [releases](https://github.com/maticnetwork/bor/releases) to find the latest stable version of bor.
    ```shell
    curl -L https://raw.githubusercontent.com/maticnetwork/install/main/bor.sh | bash -s -- v0.4.0 <network> <node_type>
    ```

The network accepts `mainnet` or `mumbai` and the node type accepts `validator` or `sentry` or `archive`.

### Building from source

- Install Go (version 1.19 or later)
- Clone the repository and build the binary using the following commands:
    ```shell
    make bor
    ```
- Start bor using the config files for validator and sentry provided in the `packaging` folder.
    ```shell
    ./build/bin/bor server --config ./packaging/templates/mainnet-v1/sentry/sentry/bor/config.toml
    ```

## License

The go-ethereum library (i.e. all code outside of the `cmd` directory) is licensed under the
[GNU Lesser General Public License v3.0](https://www.gnu.org/licenses/lgpl-3.0.en.html),
also included in our repository in the `COPYING.LESSER` file.

The go-ethereum binaries (i.e. all code inside of the `cmd` directory) are licensed under the
[GNU General Public License v3.0](https://www.gnu.org/licenses/gpl-3.0.en.html), also
included in our repository in the `COPYING` file.

<hr style="margin-top: 3em; margin-bottom: 3em;">

## Join our Discord server

Join Polygon community  – share your ideas or just say hi over [on Discord](https://discord.gg/zdwkdvMNY2).
