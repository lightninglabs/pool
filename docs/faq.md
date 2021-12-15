---
description: >-
  Lightning Pool is a non-custodial, peer-to-peer marketplace for Lightning node
  operators to buy and sell inbound channel liquidity.
---

# FAQs

## General

### What happens when I buy inbound channel liquidity?

A channel is opened to you after paying an upfront premium for inbound channel liquidity of a specific size for a set duration of time \(2016 blocks\). Once the upfront premium is paid a channel is opened to the buyer. 

### How is the price of inbound channel liquidity determined?

Set your buy price to according to the time you are willing to wait for a match and of course your own internal cost and benefit analysis.

If there is a market match, you will pay the lower of the two prices between the bid and the ask.

### How much will I pay for a purchased channel?

You pay based on a percentage across time and it is not quite straightforward to describe the cost, so allow the software to calculate the cost for you and you will be able to confirm it.

If you want to pay a specific amount, use trial and error of creating bids until the confirmation shows you your desired fee payment size.

It will not show you an exact total, but you can see how much you will pay in fee to the seller in the confirmation, in the confirmation the seller is called the “maker”.

### How will I know if my order was fulfilled?

You can check the status of your order at any time by running the following command: 

```text
pool orders list
```

### How soon after my order is filled will the seller open a channel to me?

A channel will be opened as soon as the batch transaction has received a sufficient number of confirmations. Batches are processed whenever the market clears or approximately every 10 minutes or whichever is longer.

The seller has to be online when the batch clears or the match won't be included.

### Can I buy a channel for someone else?

Yes, [sidecar channels](https://lightning.engineering/posts/2021-05-26-sidecar-channels/) solve this problem by enabling a third party to purchase channels on behalf of a user.

### Troubleshooting

[Join us on Slack](https://lightning.engineering/slack.html) and we'd be happy to help in any way we can.

## Channels

### How is the channel opened?

The channel is opened automatically by the seller once the market clears.

### How long will the channel stay open?

You can specify the desired lease period in your orders. With a minimum of 2016 blocks \(approximately 2 weeks\).

### Does the channel close after the maturity date?

No, it will remain open but the seller has no obligation to keep the channel open beyond the maturity date.

### Is there a way to ensure that the channels are of a certain size?

You can specify a minimum allowable channel size which will ensure that you receive one or more inbound channels greater than or equal to the size you specified.

### Are there limits on what I can do with the channel?

No. The channel behaves like a regular channel.

### What happens if the seller force closes a channel before the maturity date?

Script level enforcement enables us to lock the maker's funds in the channel for the lease period. However, you need an LND node that supports this feature and you need to opt in. By default, we use a feature in `lnd` to prevent channels from being _cooperatively closed_ by the maker until the expiry height \(what we call the `thaw_height`\).

### If off-chain funds have moved over to my side, must I keep them there?

If you cannot or do not want to move the funds out of the channel off-chain, you can close the channel before the term limit with no penalties.

## Accounts

### How do I create an account?

Opening an account requires first funding your `lnd` wallet and subsequently funding a time-locked 2-of-2 multi-sig account. This account is then debited whenever an order of is fully or partially matched.

When creating an account you must specify a time-lock so that you can gain access to your funds in the event that Pool is offline for a period of time. The account will expire once the specified amount of time has elapsed. 

### Why do accounts expire?

This is necessary so that users can recover funds in the event our servers were to go offline for a period of time.

### What do I do if my account expires?

You can renew expired accounts with the `pool accounts renew [command options] trader_key sat_per_vbyte [--expiry_height | --expiry_blocks]`. This will broadcast a chain	transaction as the expiry is enforced within the account output script.

## Matching

### Can I choose who to buy from?

You are not able to choose who to buy from but by default only well-connected nodes who are on the Bos Score List are eligible to sell liquidity. You can disable this restriction setting the `--min_node_tier=0` parameter when submiting an order. 

### Can sellers choose their buyers or blacklist certain buyers?

A seller cannot avoid selling to a specific peer. All peers on the network are valid buyers.

### Will I be matched to Tor nodes as a clearnet buyer?

Yes. They will be responsible for connecting to your node.

### What is my recourse if the peer goes offline or has other issues?

In the future this node may be removed from the market.

## Fees

### What are the fees for using Pool?

Buyers and sellers of channels should expect service charges of 5-25 basis points each.

A one time payment of 1000 satoshi is also required to obtain an
[LSAT](https://lsat.tech) token to communicate with the auction server. That
token currently does not expire and can therefore be used indefinitely.

### Who pays the on-chain fees?

When the channel is opened, the cost of opening the channel is split between the
buyer and seller. When closing the channel, the rules are the same as with any
other LN channel: The initiator (in the Pool case the seller) pays the closing
fee.

This will be different with anchor output channels however, once they are rolled
out by default (they're still experimental and need to be opt in). With those,
the party initiating the force close will pay the chain fee for doing so.

### Who earns the routing fees from the channel that is opened with me?

The seller earns the routing fees. You can also earn fees if that new channel is used by a routing node.

### Can the seller increase their fee rate after opening the channel?

Yes they can adjust their routing policy. However, this will be penalized in the future.

### Can I specify the maximum fee rate of a peer I’m buying liquidity from?

Not at this time but this is something we plan to add in the future.

## Technical questions

### How do I buy a channel?

```text
orders submit bid --amt 100000000 --acct_key 03b5ef9a2ab19502fbb1f72d597d772eff1db1c9f8713fbe0685009a882f7c01b6 --max_batch_fee_rate 253 --interest_rate_percent 0.0001
```

* `amt` is the total amount of inbound liquidity being bid on.
* Key is the “trader key” from “accounts list”
* Max batch fee rate is what you want to pay as part of the channel creation \(you will pay for half of the open fees as the buyer\)

### How do I see the channel I bought?

```text
accounts leases
```

* This includes the capacity: `channel_amt_sat`
* How much you paid to the seller: `premium_sat`
* How much you paid to the service: `execution_fee_sat`

### Do I need to keep `poold` running the whole time?

To participate in an auction, the `poold` trader daemon needs to be in constant communication with the auction server to validate and then sign potential batch transactions.

Once all orders are in a final state \(either fully matched or canceled\), the trader daemon can be safely shut down.

### I want to move `poold` to another machine, what files do I need to move?

As long as there are no differences in the operating system or the processor
architecture, moving `poold` to another machine is as easy as moving the `.pool`
directory.

However, because `poold` doesn't have any private keys on its own, it always has
to be connected to the same `lnd` node. Moving `poold` between nodes is not
supported and will result in errors. If you need to use a different `lnd` node,
cancel all orders and close all accounts first, then start a fresh `poold` with
a new `lnd` instance.
