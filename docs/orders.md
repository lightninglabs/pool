# Orders

## Overview

Now that we have our account set up and funded, it's time to trade some channels!

There are two types of orders in the current version of Pool: asks, and bids. You submit an ask when you have some coins that you want to _lease out_ as inbound liquidity for a certain period of time \(expressed in blocks\), at a fixed rate compounded per block. You submit a bid when you need to acquire inbound liquidity \(ability to receive\), for a given amount of time \(again expressed in blocks\), paying out a fixed rate that compounds per-block.

In the alpha version of Lightning Pool, a single lump sum premium is paid after order execution. In future versions, we plan on introducing "coupon channels" which allow for _streaming interest_ to be paid out.

One important aspect of the market is that rather than buy/sell satoshis, we use _units_. A unit is simply 100,000 satoshis and represents the _smallest_ channel that can be bought or sold on the auction platform.

With that said, let's place some orders to try to earn some yield from this 0.5 BTC that's been burning a hole in our SD card for the past year. We'll place a single order for 10 million satoshis, wanting to receive 0.3% \(30 bps\) over a 2016 block period \(approximately 2 weeks\):

```text
$ pool orders submit ask 10000000 0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732 --interest_rate_percent=0.3 --lease_duration_blocks=2016

-- Order Details --
Ask Amount: 0.1 BTC
Ask Duration: 2016
Total Premium (yield from taker): 0.00029998 BTC 
Rate Fixed: 1488
Rate Per Block: 0.000001488 (0.0001488%)
Execution Fee:  0.00010001 BTC
Max batch fee rate: 100 sat/vByte
Max chain fee: 0.0016325 BTC
Confirm order (yes/no): yes
{
        "accepted_order_nonce": "e3dbd57e8c22e895cbe705a4ec5ce65bdd766de4fc6d90c8bed404da8e7cbebc"
}
```

By leaving off the `--force` flag, we request the final break down to confirm the details of our order before we put it through.

In this case, if this order is executed, then I'll gain 29k satoshis:

```text
premium = (rate_fixed / billion) * amount * blocks
29,998 = (1,488/1,000,000,000)*10,000,000*2,016
```

It's important to note that although internally we use a fixed rate per block to compute the final premium, on the command line, we accept the final acceptable premium as a _percentage_. Therefore, when submitting orders, one should place the value that they wish to receive or accept at the end of the lease period. Internally, we'll then compute the _per block lease rate_ and submit the order using _that_.

The duration and fixed rate \(the percentage\) are two important values to pay attention to when placing orders. Given the same amount, and fixed rate, you earn more by leasing out the funds for a _longer_ period of time. Conversely, a taker will pay more if they need the funds for a longer period of time.

Also notice the _**max batch fee rate**_ break down, that regulates the _highest_ chain fee you're willing to pay to get into a batch. When traders are included in a batch, they split the channel open fee with the party they're matched with, then pay for their account to be spent and re-created. The auctioneer then uses this value during match making to ensure that traders don't pay more _chain fees_ than they intend to. If your desired chain fee is _below_ the current proposed batch chain fee, then your order won't be eligible for execution until chain fees come down somewhat.

Users can use the `--max_batch_fee_rate` value to regulate chain fees.

Take note of the `order_nonce`, it's used through the auction to identify orders, and also for authentication purposes.

We can then check out the order we just placed with the following command:

```text
$ pool orders list

{
...
    "details": {
            "trader_key": "024010a572c9f89b78b9d024f2be6cd4d42f4b9a80bfee4a5855e85da128c78473",
            "rate_fixed": 1488,
            "amt": "10000000",
            "max_batch_fee_rate_sat_per_kw": "25000",
            "order_nonce": "e3dbd57e8c22e895cbe705a4ec5ce65bdd766de4fc6d90c8bed404da8e7cbebc",
            "state": "ORDER_SUBMITTED",
            "units": 100,
            "units_unfulfilled": 100,
            "reserved_value_sat": "10143270",
            "creation_timestamp_ns": "1602679458771936428",
            "events": [
            ],
            "min_units_match": 10
    },
    "lease_duration_blocks": 2016,
    "version": 1
...
}
```

The order hasn't been cleared yet \(state `ORDER_SUBMITTED`\), and it shows up as 100 units, or 10 million satoshis.

If we instead wanted to _buy_ inbound bandwidth, we could submit a bid instead. A trader can have multiple unfilled bids and asks. Partial matching is possible as well, so someone could only purchase 10 of the 100 units we have for sale. Over time the orders will gain additional constraints such as fill-or-kill, or min partial match size.

## Detailed field breakdown

Let's now take a look at all the order specific command line flags that are available.

### Ask orders

Command for submitting an ask order:

```text
$ pool orders submit ask
```

Help output:

```text
$ pool orders submit ask --help

NAME:
   pool orders submit ask - offer channel liquidity

USAGE:
   pool orders submit ask [command options] amt acct_key [--rate_fixed=R] [--max_batch_fee_rate=F] [--lease_duration_blocks=M]

DESCRIPTION:

  Create an offer to provide inbound liquidity to an auction participant
  by opening a channel to them for a certain time.

OPTIONS:
   --interest_rate_percent value  the total percent one is willing to pay or accept as yield for the specified interval (default: 0)
   --amt value                    the amount to offer for channel creation in satoshis (default: 0)
   --acct_key value               the account key to use to offer liquidity from
   --lease_duration_blocks value  the number of blocks that the liquidity should be offered for (default: 2016)
   --min_chan_amt value           the minimum amount of satoshis that a resulting channel from this order must have (default: 0)
   --force                        skip order placement confirmation
   --max_batch_fee_rate value     the maximum fee rate (sat/vByte) to use to for the batch transaction (default: 100)
```

NOTE: The default values shown in the command line help are different from the actual default values that are used. A value of `0` on the command line indicates: _No actual value set, use the internal default value_. See the table below for more information.

| Flag | Required | Default Value | Description |
| :--- | :--- | :--- | :--- |
| `interest_rate_percent` | Yes | n/a | The interest rate that should be earned over **the total lease duration**. |
| `amt` | Yes | n/a | The amount of liquidity to offer in satoshis. Must be a multiple of the base unit \(100k sat\). |
| `acct_key` | Yes | n/a | The account's trader key to use to pay for the offered liquidity, the order submission fee and chain fees. |
| `lease_duration_blocks` | No | `2016` | The minimum number of blocks the offered channels need to stay open for in order to satisfy the contract. Distinct markets are available for the different durations. See [lease duration section](orders.md#lease-duration) for more information. |
| `min_chan_amt` | No | 10% of `amt` | The minimum size/capacity of any offered channel. Higher values reduce the match potential but decrease the potential total in chain fees that must be paid. Must be a multiple of the base unit \(100k sat\). See [chain fees section](orders.md#chain-fees) for more information. |
| `max_batch_fee_rate` | No | `100` sat/vByte | The maximum on-chain fee rate at which this order should be eligible to be included in a batch. If the auctioneer estimates a higher fee rate, orders below will be skipped. See [chain fees section](orders.md#chain-fees) for more information. |
| `force` | No | `false` | When set to `true`, no order details will be shown and no confirmation is required. |

### Bid orders

Command for submitting a bid order:

```text
$ pool orders submit bid
```

Help output:

```text
NAME:
   pool orders submit bid - obtain channel liquidity

USAGE:
   pool orders submit bid [command options] amt acct_key [--rate_fixed=R] [--max_batch_fee_rate=F] [--lease_duration_blocks=M]

DESCRIPTION:

  Place an offer for acquiring inbound liquidity by lending
  funding capacity from another participant in the order book.

OPTIONS:
   --interest_rate_percent value  the total percent one is willing to pay or accept as yield for the specified interval (default: 0)
   --amt value                    the amount of inbound liquidity in satoshis to request (default: 0)
   --acct_key value               the account key to use to pay the order fees with
   --lease_duration_blocks value  the number of blocks that the liquidity should be provided for (default: 2016)
   --min_node_tier value          the min node tier this bid should be matched with, tier 1 nodes are considered 'good', if set to tier 0, then all nodes will be considered regardless of 'quality' (default: 0)
   --min_chan_amt value           the minimum amount of satoshis that a resulting channel from this order must have (default: 0)
   --force                        skip order placement confirmation
   --max_batch_fee_rate value     the maximum fee rate (sat/vByte) to use to for the batch transaction (default: 100)
```

NOTE: The default values shown in the command line help are different from the actual default values that are used. A value of `0` on the command line indicates: _No actual value set, use the internal default value_. See the table below for more information.

| Flag | Required | Default Value | Description |
| :--- | :--- | :--- | :--- |
| `interest_rate_percent` | Yes | n/a | The maximum interest rate that should be paid for leasing a channel, calculated over **the total lease duration**. |
| `amt` | Yes | n/a | The amount of liquidity to lease in satoshis. Must be a multiple of the base unit \(100k sat\). |
| `acct_key` | Yes | n/a | The account's trader key to use to pay for the lease premium, order submission fee and chain fees. |
| `lease_duration_blocks` | No | `2016` | The minimum number of blocks the leased channels must stay open for in order to satisfy the contract. Distinct markets are available for the different durations. See [lease duration section](orders.md#lease-duration) for more information. |
| `min_chan_amt` | No | 10% of `amt` | The minimum size/capacity of any leased channel. Higher values reduce the match potential but decrease the potential total in chain fees that must be paid. Must be a multiple of the base unit \(100k sat\). See [chain fees section](orders.md#chain-fees) for more information. |
| `min_node_tier` | No | `1` | The minimum quality of node this bid should be matched with. The default \(if no command line flag is set\) is "Tier 1" which means the bid is only matched with asks from nodes that are considered "good". When manually setting this to `--min_node_tier=0` then asks from all nodes should be considered, regardless of their "quality". |
| `max_batch_fee_rate` | No | `100` sat/vByte | The maximum on-chain fee rate at which this order should be eligible to be included in a batch. If the auctioneer estimates a higher fee rate, orders below will be skipped. See [chain fees section](orders.md#chain-fees) for more information. |
| `force` | No | `false` | When set to `true`, no order details will be shown and no confirmation is required. |

## Lease duration

The lease duration dictates the duration of the contract that the maker \(submitter of the ask order\) and the taker \(submitter of the bid order\) enter when being matched by the auctioneer. That means that the maker must provide the offered liquidity **at least** for the selected number of blocks. The maker is not allowed to close the channel before the agreed upon lease duration has elapsed. The maker **may** close the channel after the duration is over but is not obligated to do so. If a channel is economically viable, further routing fees might be collected when keeping the channel open.

In contrast, the taker of the channel is always allowed to close the channel. They pay the premium for the whole duration upfront, and it is therefore to their own disadvantage to close the channel prematurely.

To allow the formation of distinct premium rates over different durations, the possible lease durations are fixed and defined by the auctioneer. The current list of possible lease durations can be queried by running the following command:

```text
$ pool auction leasedurations

{
      "lease_durations": {
              "2016": true,
              "4032": false,
      }
}
```

The integer value is the lease duration in blocks. The boolean value indicates whether matchmaking in the given duration market is currently enabled or not. When adding a new lease duration the auctioneer might not enable matchmaking right away to wait for the order book to be populated sufficiently first.

## Order execution fees

To compensate the auctioneer server for the service it is providing, an order execution fee has to be paid for every successfully executed \(partial\) match.

That execution fee is split into a static part \(the _base fee_ in satoshis\) that is always the same independent of the matched size, and the dynamic part \(the _fee rate_ in parts per million\) that is calculated based on the number of matched units.

The fee is set by the server and can change depending on demand, chain fee climate or other operational costs. The current fee can be queried by running the following command:

```text
$ pool auction fee

{
        "execution_fee": {
                "base_fee": "123",
                "fee_rate": "1234"
        }
}
```

Based on these **example** values, a matched order of 7 units \(700k satoshis\) would be charged an execution fee of 986 satoshis:

```text
execution_fee = base_fee + (fee_rate / million) * match_amount
986.8 = 123 + (1,234 / 1,000,000) * 700,000
```

## Chain fees

Each successfully executed order will result in at least one channel output being created in the batch transaction that is published to the Bitcoin network. Additionally, because an account must be spent for an order to be executed, an input per involved account is also added to the transaction, and, if there is a remaining balance, the change goes back to a newly created output for that account.

In an example worst-case-scenario, a bid order for buying one channel can result in one input and two outputs being created on-chain. Each trader is charged fully for the transaction weight they produce with their account inputs and outputs. The cost for the new channel output is split in half between the maker and the taker.

Assuming we have a bid order over 5 units \(500k satoshis\) and an account that is 10 times that size, we'd occupy 162 virtual bytes of chain space:

```text
size_vBytes = acct_input_size + 0.5 * chan_output_size + acct_output_size
162 ~= ~98 + 0.5 * 43 + 43
```

Assuming the auctioneer estimates a current network fee rate of 34 sat/vByte, that order would cost `162 * 34 = 5,508` satoshi in chain fees to execute.

The auctioneer always tries to be economical with its fee estimation, but fee estimation is hard, and the fee market remains unpredictable. To give traders some protection against excessive fees, the `--max_batch_fee_rate` can be set for any order submitted that communicates to the auctioneer that the order shouldn't be considered for matching if the current estimation is higher than that rate.

### Max chain fee estimation

When submitting an order, the command line shows a summary of the expected fees, as shown in our initial example:

```text
Execution Fee:  0.00010001 BTC
Max batch fee rate: 100 sat/vByte
Max chain fee: 0.0016325 BTC
```

The value shown as `Max chain fee` can look pretty scary. But it is important to note that the value is considered to be the **absolute worst-case fee**.

The maximum chain fee is calculated based on how many potential partial matches an order could go through \(assuming it would always only match the minimum match size or minimum channel size\). And for each of those partial matches the chain fee calculation as shown in the previous section would be performed with the given `Max batch fee rate`. It is quite unlikely however that the maximum fee is ever required to be paid for an order to be executed.

To reduce the value of this worst-case estimation, there are two possible flags that can be used to influence it:

* `--min_chan_amt`: The minimum match size or minimum channel size. Instructs

  the auctioneer to only match the order with a counterpart that can at least

  fill the given amount in one match. By default, this values is set to 10% of

  the full order amount. Increasing the value makes it less likely to find a

  match candidate but reduces the number of times chain fees have to be paid.

  Increasing the value to 100% of the order amount is equivalent to a

  "kill-or-fill" order that is either executed in one part or not at all.

* `--max_batch_fee_rate`: The maximum network fee rate the order should be

  considered for. Orders with a value higher than what the auctioneer estimates

  at the time of matchmaking will be excluded for that batch. Setting this

  value too low might result in orders not being executed at all or only when

  the mempools are almost empty.
  
## Cancelling orders

To cancel an order. First get the order_nonce of the to be canceled order by using the list command and then cancel it: 

```text
$ pool orders cancel order_nonce
```

## Extensibility

At the moment there are only two restrictions that users can set on _who_ their orders will be matched against. There is the global `--newnodesonly` flag which will cause the trader daemon to reject any matches with nodes that its connected `lnd` node already has channels with.

Takers can additionally influence the "quality" of the nodes they want their bid orders to be matched against by setting the `--min_node_tier` when creating the order.

In the future, more matching restrictions will likely be implemented, for example:

* Specifying a concrete list of `lnd` node public keys that an order should

  be matched with.

* Instant order: Either be matched successfully in the next batch or be

  canceled.

* Buying channels for someone else \(a.k.a. "Sidecar Channels"\): When creating

  a bid order, a third party's `lnd` node pubkey can be specified that will

  receive the leased channel instead of the node of the bidder.

To allow these future upgrades to work seamlessly, a version field was added to the order submission protocol from the beginning. With this version field, the trader and auctioneer always know what fields an order is supposed to have set and therefore what fields are covered by the trader's order signature.

