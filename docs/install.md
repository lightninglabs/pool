# Installation

## Installation

Lightning Pool is built very similarly to [Lightning Loop](https://github.com/lightninglabs/loop): There is a process that is constantly running in the background, called the trader daemon \(`poold`\) and a command line tool to interact with the daemon, called just `pool`.

The `poold` trader daemon can be run either as a standalone binary connected to a compatible `lnd` node or integrated into [Lightning Terminal \(LiT\)](https://github.com/lightninglabs/lightning-terminal).

### Downloading the standalone binaries

The latest official release binaries can be [downloaded from the GitHub releases page](https://github.com/lightninglabs/pool/releases).

### Downloading as part of Lightning Terminal \(LiT\)

To run `poold` integrated into the Lightning Terminal, download [the latest release of LiT](https://github.com/lightninglabs/lightning-terminal/releases) and follow [the installation instructions of LiT](https://github.com/lightninglabs/lightning-terminal#execution)

### Building the binaries from source

To build both the `poold` and `pool` binaries from the source code, at least the `go 1.14` and `make` must be installed.

To download the code, compile and install it, the following commands can then be run:

```text
$ git clone https://github.com/lightninglabs/pool
$ cd pool
$ make install
```

This will install the binaries into your `$GOPATH/bin` directory.

### Installing `lnd`

Lightning Pool needs to be connected to an `lnd` node version `v0.12.0-beta` (`v0.13.3-beta` recommended!) or later to work. It is recommended to run an [official release binary of `lnd`](https://github.com/lightningnetwork/lnd/releases).

[Installing `lnd` from source](https://github.com/lightningnetwork/lnd/blob/master/docs/INSTALL.md#installing-lnd) is also possible but needs to be done **with all sub-server build flags enabled**:

```text
$ make install tags="signrpc walletrpc chainrpc invoicesrpc"
```

### Running `poold`

If `lnd` is configured with the default values and is running on the same machine, `poold` will be able to connect to it automatically and can be started by simply running:

```text
$ poold

# Or if you want to do everything in the same terminal and run poold in the
# background:
$ poold &

# For testnet mode, you'll need to specify the network as mainnet is the
# default:
$ poold --network=testnet
```

In the case that `lnd` is running on a remote node, the `tls.cert` and the `admin.macaroon` files from the `lnd` data directory need to be copied to the machine where `poold` is running.

The daemon can then be configured to connect to the remote `lnd` node by using the following command line flags:

```text
$ poold --lnd.host=<the_remote_host_IP_address>:10009 \
        --lnd.macaroonpath=/some/directory/with/lnd/data/macaroons/admin.macaroon \
        --lnd.tlspath=/some/directory/with/lnd/data/tls.cert
```

To persist this configuration, these values can also be written to a configuration file, located in `~/.pool/<network>/poold.conf`, for example:

> ~/.pool/mainnet/poold.conf
>
> ```text
> lnd.host=<the_remote_host_IP_address>:10009
> lnd.macaroonpath=/some/directory/with/lnd/data/macaroons/admin.macaroon
> lnd.tlspath=/some/directory/with/lnd/data/tls.cert
> ```

### Configuration options

There is a range of operational settings that can be set to change the default logging behavior or change the directories where `poold` stores its data. To see the full list of options, run `poold --help`.

The following list only includes flags that have an impact on the match making or business related behavior of the Pool trader daemon:

| Flag | Required | Default Value | Description |
| :--- | :--- | :--- | :--- |
| `newnodesonly` | No | `false` | If set to `true` the daemon will only buy channels from nodes it does not yet have channels with |

## Authentication and transport security

The gRPC and REST connections of `poold` are encrypted with TLS and secured with macaroon authentication the same way `lnd` is.

If no custom base directory is set then the TLS certificate is stored in `~/.pool/<network>/tls.cert` and the base macaroon in `~/.pool/<network>/pool.macaroon`.

The `pool` command will pick up these file automatically on mainnet if no custom base directory is used. For other networks it should be sufficient to add the `--network` flag to tell the CLI in what sub directory to look for the files.

For more information on macaroons, [see the macaroon documentation of lnd.](https://github.com/lightningnetwork/lnd/blob/master/docs/macaroons.md)

**NOTE**: pool's macaroons are independent from `lnd`'s. The same macaroon cannot be used for both `poold` and `lnd`.

