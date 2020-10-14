# Orders

## Overview

Now that we have our account set up and funded, it's time to trade some channels!

There're two types of orders in the current version of Pool: asks, and bids. You submit an ask when you have some coins that you want to _lease out_ as inbound liquidity for a maximum period of time \(expressed in blocks\), at a fixed rate compounded per block. You submit a bid when you need to acquire inbound liquidity \(ability to receive\), for a minimum amount of time \(again expressed in blocks\), paying out a fixed rate that compounds per-block.

In the alpha version of Lightning Pool, a single lump sum premium is paid after order execution. In future versions, we plan on introducing "coupon channels" which allow for _streaming interest_ to be paid out.

One important aspect of the market is that rather than buy/sell satoshis, we use _units_. A unit is simply 100,000 satoshis and represents the _smallest_ channel that can be bought or sold on the network.

With that said, let's place some orders to try to earn some yield from this 0.5 BTC that's been burning a hole in our SD card for the past year. We'll place a single order for 10 million satoshis, wanting to receive 0.3% \(30 bps\) over a 3000 block period \(a bit under 3 weeks\):

```text
🏔 pool orders submit ask 10000000 0288096be9917f8ebdfc6eb2701635fe658f4eae1e0274dcce41418b3fb5145732 --interest_rate_percent=0.3 --max_duration_blocks=3000
-- Order Details --
Ask Amount: 0.1 BTC
Ask Duration: 3000
Total Premium (yield from taker): 0.0003 BTC
Rate Fixed: 1000
Rate Per Block: 0.000001000 (0.0001000%)
Execution Fee:  0.00010001 BTC
Max batch fee rate: 25000 sat/kw
Max chain fee: 0.016325 BTC
Confirm order (yes/no): yes
{
        "accepted_order_nonce": "f1bebca6047dee6657f82377ebac94d1dc6667097f2a4d463deb63eff6f0dbcf"
}
```

By leaving off the `--force` flag, we request the final break down to confirm the details of our order before we put it through.

In this case, if this order is executed, then I'll gain 30k satoshis:

```text
premium = (rate_fixed / billion) * amount * blocks
30,000 = (1,000/1,000,000,000)*1,000,000*3,000
```

It's important to note that although internally we use a fixed rate per block to compute the final premium, on the command line, we accept the final acceptable premium as a _percentage_. Therefore, when submitting orders, one should place the value that they wish to receive or accept at the end of the lease period. Internally, we'll then compute the _per block lease rate_ and submit the order using _that_.

The duration and fixed rate \(the percentage\) are two important values to pay attention to when placing orders. Given the same amount, and fixed rate, you earn more by leasing out the funds for a _longer_ period of time. Conversely, a taker will pay more if they need the funds for a longer period of time.

Also notice the +_max batch fee\*_ break down, that regulates the _highest_ chain fee you're willing to pay to get into a batch. When traders are included in a batch, they split the channel open fee with the party they're matched with, then pay for their account to be spent and re-created. The auctioneer then uses this value during match making to ensure that traders don't pay more _chain fees_ than they intend to. If your desired chain fee is _below_ the current proposed batch chain fee, then your order won't be eligible for execution until chain fees come down somewhat.

Users can use the `--max_batch_fee_rate` value to regulate chain fees. Note that the values is expressed in `sat/kw` on the command line. To convert from `sat/vbyte` to `sat/kw`, simply _divide_ by `250`.

Take note of the `order_nonce`, it's used through the auction to identify orders, and also for authentication purposes.

We can then check out the order we just placed with the following command:

```text
🏔 pool orders list
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

The order hasn't been cleared yet \(state `ORDER_SUBMITTED`\), and it shows up as 100 units, or 10 million satoshis.

If we instead wanted to _buy_ inbound bandwidth, we could submit a bid instead. A trader can have multiple unfilled bids and asks. Partial matching is possible as well, so someone could only purchase 10 of the 100 units we have for sale. Over time the orders will gain additional constraints such as fill-or-kill, or min partial match size.

