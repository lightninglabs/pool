# Accounts

## Overview

Like any exchange/auction, before you can start trading, you'll need an account! Accounts in the Pool are actually special on-chain contracts. A user deposits a certain amount of funds into an account which has a set expiry. By having users commit funds to an account in order to place orders, we ensure that they're unable to spoof orders \(placing an order that they can't fulfill\). We also add a cost to attempting to sybil attack the venue as well.

The script for an account is very simple, funds can be moved from the account:

* With a joint 2-of-2 signature by the _auctioneer_ and the user.
* Unilaterally by the user after the expiration period has passed.

This script resembles certain two-factor wallets with a time-lock escape clause. The second clause ensures that users are able to move their funds if the auctioneer is unavailable.

Many interactions in Pool are based around accounts:

* Fees paid to the auctioneer are deducted from your account
* Fees gained by selling channels are credited to your account
* Funds used to open channels to others are deducted from your account

As an account is just a UTXO, anytime a batch is cleared in the auction \(market made, channels bought+sold\), your account is spent, and re-created in the same transaction. This applies to other actions like
deposit/withdraw funds from your account.

## Creating An Account

Creating an account has two parameters: the size of the account, and the expiry of an account. As you'll see below, both values can be adjusted at any time.

We can create an account using `pool`, like so:

```text
🏔 pool accounts new --amt=50000000 --expiry_height=1773394 --conf_target=6
{
        "trader_key": "0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732",
        "outpoint": "c6f62c80095c98a57f2eef485a7ff06611f97dc856754cad330f4eeb538ff514:0",
        "value": 50000000,
        "expiration_height": 1773394,
        "state": "PENDING_OPEN",
        "close_txid": "0000000000000000000000000000000000000000000000000000000000000000"
}
```

It's also possible to specify a _relative_ account expiry based on the current best block with the `--expiry_blocks` argument. As an example, if I wanted my account to expiry in 2 weeks, I would pass: `--expiry_blocks=2016`.

Here I created an account with 0.5 BTC, that'll expire at height `1773394`. The response shows that it's now pending open \(unconfirmed\), my `trader_key` \(used to sign orders\), and the outpoint of my new account.

Once at least 3 blocks have passed \(in the alpha\), the account will be confirmed and ready for use:

```text
🏔 pool accounts list
{
        "accounts": [
                {
                        "trader_key": "0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732",
                        "outpoint": "c6f62c80095c98a57f2eef485a7ff06611f97dc856754cad330f4eeb538ff514:0",
                        "value": 50000000,
                        "expiration_height": 1773394,
                        "state": "OPEN",
                        "close_txid": "0000000000000000000000000000000000000000000000000000000000000000"
                }
        ]
}
```

## Depositing To An Account

We can add more funds to an account using the `pool accounts deposit` command. Under the hood, we can actually batch _other_ transactions with account modifications \(make other payments, etc\), but for now we expose only the basic functionality over the CLI.

**NOTE**: You should _never_ send coins directly to your account output as it won't be picked up by the auctioneer.

Let's say I want to deposit an extra 1 million satoshis into my account, I can do so with the following command:

```text
🏔 pool accounts deposit --trader_key=0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732 --amt=1000000 --sat_per_vbyte=5
{
        "account": {
                "trader_key": "0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732",
                "outpoint": "fef5cc4936290c6d57cda83bc3e90e75270296da8f34951cd562ac4cd37d4eef:0",
                "value": 50001714,
                "expiration_height": 1773394,
                "state": "PENDING_UPDATE",
                "close_txid": "0000000000000000000000000000000000000000000000000000000000000000"
        },
        "deposit_txid": "fef5cc4936290c6d57cda83bc3e90e75270296da8f34951cd562ac4cd37d4eef"
}
```

I specify my `trader_key` explicitly, as it's possible for `poold` to manage _multiple_ accounts. The response shows my modified account, alongside with the `txid` that'll be used to service the deposit. Once this transaction has confirmed, I'll be able to use my account again.

Note that these funds came from the backing `lnd` node that `poold` is connected to. At a future time we also plan to support a traditional _deposit_ address as well.

## Withdrawing From An Account

Incrementally _withdrawing_ from an account is also supported. The command is similar to the deposit command. If I wanted to extract that 1 million from that account \(let's say it's my profit for the past week\) and send elsewhere, I can do so with the following command:

```text
🏔 pool accounts withdraw --trader_key=0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732 --amt=1000000 --sat_per_vbyte=5 --addr=tb1qe3ueyx8jhlj4h0s6mgywmtl8vlwxqkgkgp3m3s
{
        "account": {
                "trader_key": "0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732",
                "outpoint": "31664dcf5dd4e89a398e816afa55a36f7518560de08b3167d75bbc6804674cd1:1",
                "value": 49000801,
                "expiration_height": 1773394,
                "state": "PENDING_UPDATE",
                "close_txid": "0000000000000000000000000000000000000000000000000000000000000000"
        },
        "withdraw_txid": "31664dcf5dd4e89a398e816afa55a36f7518560de08b3167d75bbc6804674cd1"
}
```

## Closing An Account

Finally, if you wish to send _all_ your funds elsewhere, it's possible to close your account out before the main expiration period. We can close out the account we created above with the following command:

```text
🏔 pool accounts close --trader_key=0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732 --sat_per_vbyte 11
```
