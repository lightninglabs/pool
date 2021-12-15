# Quickstart

## Download `poold`

### Download the standalone binaries

The latest official release binaries can be [downloaded from the GitHub releases page](https://github.com/lightninglabs/pool/releases).

### Download Lightning Terminal \(LiT\)

To run `poold` integrated into the Lightning Terminal, download [the latest release of LiT](https://github.com/lightninglabs/lightning-terminal/releases) and follow [the installation instructions of LiT](https://github.com/lightninglabs/lightning-terminal#execution)

## Prepare `lnd`

Pool needs to be connected to an `lnd` node running somewhere. We assume here
that the `lnd` node is running and set up in a way that Pool can connect to it.
Consult the [installation guide](./install.md) for more information on how to
set up and configure `lnd`. Although pool supports `lnd` from version `v0.12.0-beta`, 
`v0.13.3-beta` or later is recommended.

The `lnd` node must have at least one active channel and **must be able to pay
a 1000 satoshi [LSAT fee](https://lsat.tech)**. See [the FAQ](./faq.md#fees) for
more information on this fee.

## Run `poold`

If `lnd` is configured with the default values and is running on the same machine, `poold` will be able to connect to it automatically and can be started by simply running:

```text
$ poold
```

If you're using Lightning Terminal then you can just run `litd` instead:

```text
$ litd
```

## Create an account

Creating an account has two parameters: the size of the account, and the expiry of an account. The funds for the account will be pulled from your `lnd` wallet. Create an account by running the following command:

```text
$ pool accounts new --amt=50000000 --expiry_height=1773394 --conf_target=6
```

Once at least 3 blocks have passed \(in the alpha\), the account will be confirmed and ready for use:

Run the following command to view account details:

```text
$ pool accounts list
```

## Submit an order

There are two types of orders in the current version of Pool: asks, and bids. You submit an ask when you have some coins that you want to _lease out_ as inbound liquidity for a certain period of time \(expressed in blocks\), at a fixed rate compounded per block. You submit a bid when you need to acquire inbound liquidity \(ability to receive\), for a given amount of time \(again expressed in blocks\), paying out a fixed rate that compounds per-block.

To submit a bid:

```text
$ pool orders submit bid
```

Be sure to add the following flags:

| Flag | Description |
| :--- | :--- |
| `interest_rate_percent` | The interest rate that should be earned over **the total lease duration**. |
| `amt` | The amount of liquidity to offer in satoshis. Must be a multiple of the base unit \(100k sat\). |
| `acct_key` | The account's trader key to use to pay for the offered liquidity, the order submission fee and chain fees. |

We can then check out the order we just placed with the following command:

```text
$ pool orders list
```

## Matched Orders

Once an order has been matched in an auction, the `pool auction leases` command can be used to examine your current set of purchased/sold channel leases. An example output looks something like the following:

```text
$ pool auction leases
{
        "leases": [
                {
                        "channel_point": "78cc6879c1dc1c00f22b29a06458f1335ed0fdb7d05c01b9077e3155e697bbb9:2",
                        "channel_amt_sat": 5000000,
                        "channel_duration_blocks": 144,
                        "premium_sat": 40000,
                        "execution_fee_sat": 5001,
                        "chain_fee_sat": 165,
                        "order_nonce": "eb972cd21cf1651e251c8b07d69e89c47294bae104fbdc9da26edf9aee335c9a",
                        "purchased": false
                }
        ],
        "total_amt_earned_sat": 40000,
        "total_amt_paid_sat": 5166
}
```

Here we can see that a channel sold for 40k satoshis, and ended up paying 5k satoshis in chain and execution fees, netting a cool 35k satoshi yield. Within the actual auction, these numbers will vary based on the chain fee rate, the market prices, and also the execution fees. Users can constraint how much chain fees they'll pay by setting the `--max_batch_fee_rate` argument when submitting orders.

