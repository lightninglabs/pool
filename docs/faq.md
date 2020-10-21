---
description: >-
  Pool is a non-custodial marketplace for Lightning channels. Lightning Pool
  enables new participants to rent Lightning channels so they can instantly
  start accepting payments on the Lightning Network.
---

# FAQ

## General questions

### How do lease payments for channels work?

You pay for the full liquidity up front, like renting an apartment but you put all the money down up front.

### How do I pay my leasing bills?

You first pay into a time-locked multi-sig and that wallet is used to pay for leasing costs.

You specify a time-lock, however if your time lock is too short then the funds can no longer be used to pay for leasing costs after they expire.

Even if you specify a long timelock, you can still get your funds out provided the server is still cooperative.

If your funds do expire, they will remain in the special wallet but they can no longer be used to pay for leases until you re-encumber them with a future lock. You can also simply withdraw these expired funds.

### Can I deposit more funds into my leasing account?

Depositing additional funds can be done but it will require spending the existing funds to combine the new deposit with the old deposit into a single UTXO.

Another option is to create a new account, which will not require a spend, but generally as a buyer, there is no particular reason to create multiple accounts.

You can choose the chain fee rate for these operations.

### How will I know if my order was fulfilled?

Watch the logs. Pull the order snapshot locally. Watch your account balance. Pool orders list. 

### How soon after my order is filled will the seller open a channel to me?

A channel will be opened as soon as the batch transaction has received N confirmations. 

If the seller’s node is offline when the batch clears, \_\_\_\_

### Do I get to choose my peer?

No, you cannot choose who you peer with. You will peer with any partner who is allowed in the market and meets your asking price.

### Who earns the routing fees from the channel that is opened with me?

The seller earns the routing fees. You can also earn fees if that new channel is used by a routing node. 

### Can I specify the maximum fee rate of a peer I’m buying liquidity from?

Not at this time but this is something we hope to add in the future.

### Can the lender increase their fee rate after opening a channel with me?

Yes they can adjust their routing policy. This will be penalized in the future.

### If I am buying inbound liquidity can I specify who the seller should open a channel to?

Not at this time but this is something we hope to add in the future.

### What motivates sellers to provide good service after purchase?

They are still able to earn routing fees on top of the channel selling fee.

### What is my recourse if the peer goes offline or has other issues?

In the future this node may be removed from the market by market participant selection, but otherwise there is no specific recourse.

### What happens if the lender force closes a channel before the maturity date?

If the lender force closes they will be banned from the market. This will not be possible in future versions. 

### What is the minimum purchase time for a channel?

You can buy a channel for one day or longer.

### Will purchased channels close after their purchase time?

No, they will remain open and become like normal channels.

If you believe your peer will not close out on you after your chosen time, you can choose a shorter time and pay a lower amount for the channel.

Even after the term, the seller of the channel will still be restricted by the normal CSV limits of the channel. A force close by the seller even after a 1 day term expiration would still result in the seller waiting for 2 weeks.

### Do sellers get to choose their buyers or blacklist certain buyers?

A seller cannot avoid selling to a specific peer. All peers on the network are valid buyers.

### Do I get to choose the size of my channels?

No, there is no way to select the channel sizes, so you can have many small channels appear.

### What is the shortest time period I can buy a channel for?

1 day. The maximum is 6 months.

### Who pays for the on-chain fees, like if I want to close my channel?

You pay for the closing on-chain fees and future UTXO consolidation costs. For the opening transaction, you can control the on-chain fee you pay, although the weight is a fixed amount relating to the sum of an input and an output, split between buyer and seller, regardless of the actual weight used to fund the channel.

### If off-chain funds have moved over to my side, must I keep them there?

If you cannot or don't want to move those funds out of the channel off-chain, you can close the channel before the term limit with no penalties.

### How much do you pay for a purchased channel?

You pay based on a percentage across time and it is not quite straightforward to describe the cost, so allow the software to calculate the cost for you and you will be able to confirm it.

If you want to pay a specific amount, use trial and error of creating bids until the confirmation shows you your desired fee payment size.

It will not show you an exact total, but you can see how much you will pay in fee to the seller in the confirmation, in the confirmation the seller is called the “maker”

### How do you choose a price?

If there is a market match, you will pay the lower of the two prices between the bid and the ask, but if there is no match, you will have to wait for the sell side to go lower.

Set your buy price to according to the time you are willing to wait for a match and of course your own internal cost and benefit analysis.

### Are there limits on what I can do with the channel?

As a buyer the channel always behaves like a regular channel and you can do anything you want with it.

### Will I be matched to Tor nodes as a clearnet buyer?

Yes, and they will be responsible for connecting to your node.

### What are the fees for using Pool?

To be determined

## Accounts

### Why do accounts expire?

This is necessary so that users can recover funds in the event we disappear.

### What do I do if my account expires?

Currently you will need to close your account and then re-open your account. In the future you will be able to renew your account as part of a batch will be the most cost efficient way of opening an account. 

## Technical questions

### How do I buy a channel?

```text
orders submit bid --amt 100000000 --acct_key 03b5ef9a2ab19502fbb1f72d597d772eff1db1c9f8713fbe0685009a882f7c01b6 --min_duration_blocks 144 --max_batch_fee_rate 253 --interest_rate_percent 0.0001
```

* Amt is the total capacity, split in as many ways as there are sell matches 
* Key is the “trader key” from “accounts list”
* Blocks is minimum 144 which is one day
* Max batch fee rate is what you want to pay as part of the channel creation \(you will pay for half of the open fees as the buyer\)

### How do I see the channel I bought?

```text
accounts leases
```

* This includes the capacity: “channel\_amt\_sat”
* How much you paid to the seller: “premium\_sat”
* How much you paid to the service: “execution\_fee\_sat”

### Do I need to keep `poold` running the whole time?

To participate in an auction, the `poold` trader daemon needs to be in constant
communication with the auction server to validate and then sign potential batch
transactions.

Once all orders are in a final state (either fully matched or canceled), the
trader daemon can be safely shut down.
