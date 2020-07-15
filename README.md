
# Lightning Liquidity Marketplace (LLM)

The Lightning Lightning Marketplace (LLM) is a non-custodial batched uniform
clearing-price auction for Channel Liquidity Bonds (CLB). A CLB packages up
inbound (or outbound!) channel liquidity (ability to send/receive funds) as a
fixed incoming asset (earning interest over time) with a maturity date
expressed in blocks. The maturity date of each of the channels is enforced by
Bitcoin contracts, ensuring that the funds of the maker (the party that sold
the channel) can't be swept until the maturity height.  All cleared orders
(purchased channels) are cleared in a single batched on-chain transaction. 

The existence of an open auction to acquire/sell channel liquidity provides all
participants on the network with a more _stable_ income source in addition to
routing network fees. By selling liquidity within the marketplace, individuals
are able to price their channels to ensure that they're compensated for the
time-value of their coins within a channel, accounting for worst-case force
close CSV delays.

The LLM critically allows participants on the network to exchange pricing
signals to determine where liquidity in the network is most _demanded_. A
channel opened to an area of the sub-graph that doesn't actually need that
liquidity will likely remain dormant and not earn any active routing fees.
Instead, if capital can be allocated within the network in an efficient manner,
being placed where it's most demanded, we can better utilize the allocated
capital on the network, and also allow new participants to easily identify
where their capital is most needed.

Amongst several other uses cases, the LLM allows a new participant in the
network to easily _boostrap_ their ability to receive funds by paying only a
percentage of the total amount of inbound funds acquired. As an example, a node
could acquire 100 million satoshis (1000 units, more on that below) for 100,000
satoshis for 0.1%. Ultimately the prices will be determined by the open market
place.

A non-exhaustive list of use cases includes:

  * **Bootstrapping new users with side car channels**: A common question
    posted concerning the Lightning Network goes something like: Alice is new
    to Bitcoin entirely, how can she join the Lightning Network without her,
    herself, making any new on-chain Bitcoin transactions? It’s desirable to a
    solution to on boarding new users on to the neowrk which is as as simple as
    sending coins to a fresh address. The LLM solves this by allowing a third
    party Carol, to purchase a channel _for_ Alice, which includes starting
    _outbound_ liquidity.

  * **Demand fueled routing node channel selection**: Another common question
    with regards to the LN is: "where should I open my channels to , such that
    they'll actually be routed through"?. The LLM provides a new signal for
    autopilot agents: a market demand signal. The node can offer up its
    liquidity and have it automatically be allocated where it's most demanded.

  * **Bootstrapping new services to Lightning**: Any new service launched on
    the Lightning network will likely need to figure out how to obtain inbound
    channels so they can accept payments. For this LLM provides an elegant
    solution in that a marchant can set up a series of "introduction points"
    negotiated via the market place. The merchant can pay a small percentage of
    the total amount of liquidity allocated towards it, and also ensure that
    the funds will be committed for a set period of time.

  * **Allowing users to instantly receive with a wallet**: A common UX
    challenge that wallets face concerns ensuring a user can receive funds as
    soon as they set up a wallet. Some wallet providers have chosen to open new
    inbound channels to users themselves. This gives users the inbound
    bandwidth they need to receive, but can come at a high capital cost to the
    wallet provider as they need to commit funds with a 1:1 ratio. The LLM
    allows them to acheive some leverage in a sense, as they can pay only a
    percentage of the funds to be allocated to a new user. As an eaxmple, they
    can pay 1000 satohis to have 1 million satoshis be alloacted to a user.

## The Auction Lifecycle

In this section, we'll walk through the typical auction lifecycle, and in the
process explain some key components of the LLM, and also illustrate how to
drive your `llmd` on the command line.

### Accounts  

Like any exchange/auction, before you can start trading, you'll need an
account! Accounts in the LLM are actually special on-chain contracts. A user
deposits a certain amount of funds into an account which has a set expiry. By
having users commit funds to an account in order to place orders, we ensure
that they're unable to spoof orders (placing an order that they can't fulfil).
We also add a cost to attempting to sybil attack the venue as well.

The script for an account is very simple, funds can be moved from the account:

  * With a joint 2-of-2 signature by the _auctioneer_ and the user.
  * Unilaterally by the user after the expiration period has passed.

This script resembles certain two-factor wallets with a time-lock escape
clause.  The second clause ensures that users are able to move their funds if
the auctioneer is unavailable.

Many interactions in CLM are based around accounts:

  * Fees paid to the auctioneer are deducted from your account
  * Fees gained by selling channels are credited to your account
  * Funds used to open channels to others are deducted from your account

As an account is just a UTXO, anytime a batch is cleared in the auction (market
made, channels bought+sold), your account is spent, and re-created in the same
transaction.

#### Creating An Account

Creating an account has two parameters: the size of the account, and the expiry
of an account. As you'll see below, both values can be adjusted at any time.

We can create an account using `llm`, like so:
```
🏔 agora accounts new --amt=50000000 --expiry_height=1773394
{
        "trader_key": "0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732",
        "outpoint": "c6f62c80095c98a57f2eef485a7ff06611f97dc856754cad330f4eeb538ff514:0",
        "value": 50000000,
        "expiration_height": 1773394,
        "state": "PENDING_OPEN",
        "close_txid": "0000000000000000000000000000000000000000000000000000000000000000"
}
```

It's also possible to specify a _relative_ account expiry based on the current
best block with the `--expiry_blocks` argument. As an example, if I wanted my
account to expiry in 2 weeks, I would pass: `--expiry_blocks=2016'.

Here I created an account with 0.5 BTC, that'll expire at height `1773394`. The
response shows that it's now pending open (unconfirmed), my `trader_key` (used
to sign orders), and the outpoint of my new account.

Once at least 3 blocks has passed (in the alpha), the account will be confirmed
and ready for use:
```
🏔 llm accounts list
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

#### Depositing To An Account

We can add more funds to an account using the `llm accounts deposit` command.
Under the hood, we can actually batch _other_ transactions with account
modifications (make other payments, etc), but for now we expose only the basic
functionality over the CLI.

**NOTE**: You should _never_ send coins directly to your account output as it
won't be picked up by the auctioneer.

Let's say I want to deposit an extra 1 million satoshis into my account, I can
do so with the following command: 
```
🏔 llm accounts deposit --trader_key=0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732 --amt=1000000 --sat_per_vbyte=5
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

I specify my `trader_key` explicitly, as it's possible for `llmd` to manage
_multiple_ accounts. The response shows my modified account, along side with
the `txid` that'll be used to service the deposit. Once this transaction has
confirmed, I'll be able to use my account again.

Note that these funds came from the backing `lnd` node that `llmd` is connected
to. At a future time we also plan to support a traditional _deposit_ address as
well.

#### Withdrawing From An Account

Incrementally _withdrawing_ from an account is also supported. The command is
similar to the deposit command. If I wanted to extract that 1 million from that
account (let's say it's my profit for the past week) and send elsewhere, I can
do so with the following command:
```
🏔 llm accounts withdraw --trader_key=0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732 --amt=1000000 --sat_per_vbyte=5 --addr=tb1qe3ueyx8jhlj4h0s6mgywmtl8vlwxqkgkgp3m3s
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

#### Closing An Account

Finally, if you wish to send _all_ your funds elsewhere, it's possible to close
your account out before the main expiration period. We can close out the
account we created above with the following command: 
```
🏔 llm accounts close --trader_key=0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732
```

### Orders

Now that we have our account set up and funded, it's time to trade some channels!

There're two types of orders in the current version of the LLM: asks, and bids.
You submit an ask when you have some coins that you want to _lease out_ as
inbound liquidity for a maximum period of time (expressed in blocks), at a
fixed rate compounded per block. You submit a bid when you need to acquire
inbound liquidity (ability to receive), for a minimum amount of time (again
expressed in blocks), paying out a fixed rate that compounds per-block.

In the alpha version of LLM, a single lump sum premium is paid after order
execution. In future versions, we plan on introducing "coupon channels" which
allow for _streaming interest_ to be paid out.

One important aspect of the market is that rather than buy/sell satoshis, we
use _units_. A unit is imply 100,000 satoshis and represents the _smallest_
channel that can be bought or sold on the network.

With that said, let's place some orders to try to earn some yield from this 0.5
BTC that's been burning a hole in our SD card for the past year. We'll place a
single order for 10 million satoshis, wanting to receive 0.3% (30 bps)
over a 3000 block period (a bit under 3 weeks):
```
🏔 llm orders submit ask 10000000 0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732 --interest_rate_percent=0.3 --max_duration_blocks=3000
Ask Amount: 0.1 BTC
Ask Duration: 3000
Total 0.0003 BTC Premium (paid to maker):
Rate Fixed: 1
Rate Per Block: 0.0000010 (0.0001000%)
Execution Fee:  0.00010001 BTC
Confirm order (yes/no): eyes
{
        "accepted_order_nonce": "f1bebca6047dee6657f82377ebac94d1dc6667097f2a4d463deb63eff6f0dbcf"
}
```

By leaving off the `--force` flag, we request the final break down to confirm
the details of our order before we put it through.

In this case, if this order is executed, then I'll gain 3k satoshis:
```
3000 = (1/100000)*1000000*3000
```

It's important to note that although internally we use a fixed rate per block
to compute the final premium, on the command line, we accept the final
acceptable premium in a _percent_. Therefore, when submitting orders, one
should place the value that they wish to receive or accept at the end of the
lease period. Internally, we'll then compute the _per block lease rate_ and
submit the order using _that_.

The duration and fixed rate (the percentage) are two important values to pay
attention to when placing orders. Given the same amount, and fixed rate, you
earn more by leasing out the funds for a _longer_ period of time. Conversely, a
taker will pay more if they need the funds for a longer period of time.

Take note of the `order_nonce`, it's used through the auction to identify
orders, and also for authentication purposes. 

We can then check out the order we just placed with the following command: 
```
🏔 llm orders list
{
    "asks": [
    {
        "details": {
                "trader_key": "0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732",
                "rate_fixed": 1,
                "amt": "10000000",
                "funding_fee_rate": "253",
                "order_nonce": "f1bebca6047dee6657f82377ebac94d1dc6667097f2a4d463deb63eff6f0dbcf",
                "state": "ORDER_SUBMITTED",
                "units": 100,
                "units_unfulfilled": 100
        },
        "max_duration_blocks": 3000,
        "version": 0,
    },
}
```

The order hasn't been cleared yet (state `ORDER_SUBMITTED`), and it shows up as
100 units, or 10 million satoshis.

If we instead wanted to _buy_ inbound bandwidth, we could submit a bit instead.
A trader can have multiple unfilled bids and asks. Partial matching is possible
as well, so someone could only purchase 10 of the 100 units we have for sale.
Over time the orders will gain additional constraints such as kill-or-fill, or
min partial match size.

### Batched Uniform-Price Clearing

Now that we have orders submitted, how does the rest of the auction actually
work? As mentioned above, LLM conducts a _discrete_ batch auction every 10
minutes. This is distinct from regular continuous exchanges in that orders are
only cleared every 10 minutes. Orders are also sealed-bid, meaning that other
traders in the venue are unable to see what others have bid. On top
of this, we utilize a uniform-clearing price algorithm to give all traders in
the batch the _same_ interest rate. This is the same mechanism used by the U.S
Treasury for its bonds, and is intended to promote fairness as your order will
only be matched with a price better than your initial ask/bid.

Note that it's possible that after the 10 minutes interval has passed a market
can't be made (supply and demand didn't cross). In this case, nothing happens,
and we just wait for the next batch to come across.

To illustrate how the uniform price clearing works consider the following
example. Let's say I want to buy 100 million satoshis (1 BTC), for at least 10
days (1440 blocks) at a price of 5% (using high numbers to make it easy to
follow). However, the _market clearing price_ (where the supply+demand curves
cross) is actually 1%. In this case I bid _more_ than the market clearing
price, but end up paying that price, as it's the best price that was possible
in that market.

A simple rule of thumb for bids and asks is as follows:

  * When I submit a bid, I'll either pay that amount or less.
  * When I submit an ask, I'll either receive that amount or more.

All orders in a batch are executed in a _single_ on-chain transaction. This
allows for thousands of channels to be bought/sold atomically in a single
block. We call the transaction that executes the orders the Batch Execution
Transaction. 

### Service Level Lifetime Enforcement

In the alpha version of LLM, _script level enforcement_ isn't yet implemented.
Script level enforcement would lock the maker's funds in the channel for the
lease period. This ensures that they can't just collect the premium (before
coupon channels) and close out the channel instantly. With script enforcement,
they would be able to close the channel (force close it), but their funds would
be unavailable until the maturity period has passed.

Instead, we've implemented a feature in `lnd` to prevent channels from being
_cooperatively closed_ by the maker until the expiry height (what we call the
`thaw_height`). Additionally, if we detect a force close by the maker of that
channel, then we'll ban them from the market for a set period of time.

# Installation

The following section assumes at least Go 1.13 is installed.

To install the primary daemon (`llmd`) and the CLI used to control the daemon,
the following command should be run: 
```
🏔  make install
```

Assuming `llmd` is now in your `$PATH`, you can start the daemon with the
following command (assuming you have a local testnet `lnd` running): 
```
🏔 llmd --network=testnet --auctionserver=clm.staging.lightningcluster.com:12010 --debuglevel=trace
```

The current server is reachable at `clm.staging.lightningcluster.com:12010`,
this may change as the alpha version progresses.
