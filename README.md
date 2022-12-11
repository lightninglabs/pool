# Lightning Pool

Lightning Pool is a non-custodial, peer-to-peer marketplace that allows node
operators that need inbound liquidity to pay node operators with available
capital to open channels in their direction while retaining full custody of
their funds. Pool’s first product is a Lightning Channel Lease - an inbound
channel with a pre-agreed duration.

Efficient capital allocation is one of the most widely felt pain points when
using the Lightning Network. Existing node operators do not have access to
pricing signals to help determine where in the network their outbound liquidity
should be allocated, and new node operators have no way to signal that they
need new inbound liquidity. Lightning Pool brings these two sides together into
a single market while allowing them to maintain custody of their funds.

Checkout our [documentation](https://pool.lightning.engineering/) to learn
more. For more [technical details, check out the technical white
paper](https://github.com/lightninglabs/pool-paper/blob/main/liquidity.pdf).

## How it works
Lightning Pool is a non-custodial auction for liquidity where bids are kept
private and trades clear periodically. Key aspects of Pool include:

- **Periodic clearing** - Market does not clear continuously, instead, it
  clears every block (or after multiple blocks, if there are no bids that match
  with existing asks).

- **Non-custodial** - Clients maintain an on-chain account that is a
  timelocked, 2-of-2 multisig with the auctioneer. These funds are fully in the
  user’s control at all times.

- **Sealed-bid** - All orders are submitted off-chain to the auctioneer, so
  bidders don’t have visibility into the bids of other participants

- **Uniform clearing price** - All participants in a batch clear at the same
  price. If your ask is for 2% annualized interest, you will receive >=2%. If
  you bid 5%, you will pay <=5%.

- **Batched execution** - Due to the account structure, the auctioneer is able
  to batch all completed orders into a single transaction, greatly reducing
  individual chain fees.

## Installation
Download the latest binaries from the
[releases](https://github.com/lightninglabs/pool/releases) page.

## LND

Note that Pool requires `lnd` to be built with **all of its subservers** and
requires running at least `v0.14.3`. Download the latest [official release
binary](https://github.com/lightningnetwork/lnd/releases/latest) or build `lnd`
from source by following the [installation
instructions](https://github.com/lightningnetwork/lnd/blob/master/docs/INSTALL.md).
If you choose to build `lnd` from source, use the following command to enable
all the relevant subservers:

```
make install tags="signrpc walletrpc chainrpc invoicesrpc"
```

## Usage
Read our [quickstart guide](https://pool.lightning.engineering/quickstart) to
learn more about how to use Pool. 

## Marketplace Fee 
Fees are calculated based on the amount of liquidity purchased. During the
mainnet alpha, fees will range from 5-25 basis points of the matched amount.

## Development
The Pool client is currently in early alpha and offers a simple command line
application.

The Pool daemon exposes a [gRPC
API](https://lightning.engineering/poolapi/index.html#pool-grpc-api-reference)
and [REST
API](https://lightning.engineering/poolapi/index.html#pool-rest-api-reference).

## Troubleshooting
[Join us on Slack](https://lightning.engineering/slack.html) and we'd be happy
to help in any way we can. In the meantime please see our
[FAQs](https://pool.lightning.engineering/faq).

## Build from source
If you’d prefer to compile from source code, you’ll need at least `go 1.17` and
`make`.

Run the following commands to download the code, compile and install Pool:

```shell
$ git clone https://github.com/lightninglabs/pool
$ cd pool
$ make install
```

This will install the binaries into your `$GOPATH/bin` directory.

## Compatibility
Lightning Pool requires `lnd` version `0.14.3-beta` or higher (`v0.15.4-beta` or
later is recommended).
