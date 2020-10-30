---
description: >-
  Lightning Pool is a non-custodial, peer-to-peer marketplace for Lightning node
  operators to buy and sell inbound channel liquidity.
---

# FAQs

## General

### What happens when I buy inbound channel liquidity?

A channel is opened to you after paying an upfront premium for inbound channel liquidity of a specific size for a set duration of time \(2016 blocks\). Once the upfront premium is paid a channel is opened to the buyer. 

### How do I buy inbound channel liquidity?



### How is the price of inbound channel liquidity determined?

Set your buy price to according to the time you are willing to wait for a match and of course your own internal cost and benefit analysis.

If there is a market match, you will pay the lower of the two prices between the bid and the ask.

### How much will I pay for a purchased channel?

You pay based on a percentage across time and it is not quite straightforward to describe the cost, so allow the software to calculate the cost for you and you will be able to confirm it.

If you want to pay a specific amount, use trial and error of creating bids until the confirmation shows you your desired fee payment size.

It will not show you an exact total, but you can see how much you will pay in fee to the seller in the confirmation, in the confirmation the seller is called the “maker”

### How will I know if my order was fulfilled?

Watch the logs. Pull the order snapshot locally. Watch your account balance. Pool orders list.

### How soon after my order is filled will the seller open a channel to me?

A channel will be opened as soon as the batch transaction has received N confirmations. Batches are processed whenever the market clears or approximately every 10 minutes or whichever is longer.

If the seller’s node is offline when the batch clears, \_\_\_\_

### Can I buy a channel for someone else?

Not at this time but this is something we plan to add in the future.

## Channels

### How does the channel get opened?

The channel is open automatically by the seller once the market clears.

### How long will the channel stay open?

It will stay open for a minimum of 2016 blocks \(approximately 2 weeks\).

### Does the channel close after the maturity date?

No, it will remain open but the seller has no obligation to keep the channel open beyond the maturity date.

### Is there a way to ensure that the channels are of a certain size?

You can specify a minimum allowable channel size which will ensure a channel greater than or equal to what you require.

### Are there limits on what I can do with the channel?

No. The channel behaves like a regular channel.

### What happens if the seller force closes a channel before the maturity date?

If the lender force closes they will be banned from the market. This will not be possible in the future.

### If off-chain funds have moved over to my side, must I keep them there?

If you cannot or don't want to move the funds out of the channel off-chain, you can close the channel before the term limit with no penalties.

## Accounts

### How do I create an account?

Opening an account requires first funding your `lnd` wallet and subsequently funding a time-locked 2-of-2 multi-sig account. This account is then debited whenever an order of is fully or partially matched.

When creating the account you specify a time-lock so that you can gain access to your funds in the event that Pool is offline for a period of time. The account will expire once the specified amount of time has elapsed. 

### Why do accounts expire?

This is necessary so that users can recover funds in the event our servers were to go offline for a period of time.

### What do I do if my account expires?

You will need to close your account and then re-open your account. In the future we will enable account renewals.

## Matching

### Can I choose who to buy from?

You are not able to choose who to buy from but by default only well-connected nodes who are on the Bos Score List are eligible to sell liquidity. 

### Can sellers choose their buyers or blacklist certain buyers?

A seller cannot avoid selling to a specific peer. All peers on the network are valid buyers.

### Will I be matched to Tor nodes as a clearnet buyer?

Yes. They will be responsible for connecting to your node.

### What is my recourse if the peer goes offline or has other issues?

In the future this node may be removed from the market.

## Fees

### What are the fees for using Pool?

Buyers and sellers of channels should expect service charges of 5-25 basis points each.

### Who pays the on-chain fees?

When the channel is opened, the cost of opening the channel is split between the buyer and seller. When the channel is closed, the cost of closing the channel and future UTXO consolidation costs are paid by the buyer. 

### Who earns the routing fees from the channel that is opened with me?

The seller earns the routing fees. You can also earn fees if that new channel is used by a routing node.

### Can the seller increase their fee rate after opening the channel?

Yes they can adjust their routing policy however this will be penalized in the future.

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

