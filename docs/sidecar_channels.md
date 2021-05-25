# Sidecar Channels

## Goal

Alice is a new user of LN and has no UTXOs but wants to use Lightning. She’s
willing to pay an onboarding fee to a wallet provider to get set up with both an
initial balance (=outbound capacity) and the ability to receive payments
(=inbound capacity).

The example in this document assumes that Alice is aware of the costs of
onboarding and is paying for them herself (which happens out of band from the
perspective of the Pool server and is only mentioned explicitly for the
completeness of the example).

With the APIs proposed to be implemented in the Pool server and client, it
should also be possible for a wallet provider to implement the flow in a way
that they cover the full costs of the onboarding and therefore “gift” the
channel and the balance to Alice. That would hide some of the parameters the API
needs to work, therefore we show the more involved example here.

## Participants

* Alice: New user, possibly using a mobile wallet, has no UTXOs but a wallet app
  that has lnd and Pool integrated as part of a unified (mobile) binary (only
  runs the Pool daemon, possibly even a "Pool light" daemon, does not need her
  own Pool account).
* Charlie: A wallet/service provider that offers the sidecar bootstrap service
  for a fee. Offers to convert fiat or BTC into an initial LN balance for new
  users, minus said fee. Has a well funded Pool account that can be used to pay
  for leases.
* Bob: An independent liquidity provider that is willing to open channels to new
  users. Has a well funded Pool account that can be used to open channels.

## Requirements

* All three participants need to run the Pool client that implements the sidecar
  functionality:
    * Alice: Must be able to register with the auctioneer by providing the
      multisig pubkey and be able to sign with it (explained later on).
    * Charlie: Must be able to submit an order that refers to a multisig pubkey
      of the receiver node.
    * Bob: Must be able to accept orders that require to set a push amount when
      opening the channel.
* No new functionality in lnd is required (though can be optimized later by
  replacing the channel acceptor with a new push amount parameter in the funding
  shim).

## Example flow (command line only)

1. Precondition: An agreement between Alice and Charlie is reached out-of-band.
   For this example, we assume Charlie is going to buy **10 units** (1m satoshi)
   of capacity from Pool for Alice. Of those 10 units, **2 units will be
   pushed** to Alice for her to gain outbound liquidity as well.
   The cost for the order submission fees, chain fees, the premium and the push
   amount are all paid from Charlie's Pool account. Whether Alice reimburses
   Charlie for those costs or not is not part of the protocol and irrelevant for
   this example.
2. ```shell
   charlie$   pool sidecar offer --capacity 1000000 --self_chan_balance 200000 --auto <rest of order details>
   {
     "sidecar_ticket": "sidecar15o1Y9oXtyKr3hs2UQho9YmJKbSmB...."
   }
   ```
   The sidecar ticket now contains the offer from Charlie to buy a 1m satoshi
   channel with a 200k initial self channel balance (=push amount) for Alice.
   With the `--auto` flag we signal that we want to leverage the existing
   auctioneer infrastructure to permit both peers to communicate with each
   other in order to finalize the ticket negotiation. 

   The addition of this "auto" mode means that both sides need to only execute
   a single and the rest of the negotiation happens in the background. If the
   `auto` flag is ommitted, then only the capacity and balance need to be
   specified, otherwise, all the other information one presents when submitting
   a full order needs to be specified.

3. Alice now needs to add her node's information:
   ```shell
   alice$   pool sidecar register sidecar15o1Y9oXtyKr3hs2UQho9YmJKbSmB
   { 
     "sidecar_ticket": "sidecar15o1Y9oXtyKr3hs2UQho9YmJKbSmB...."
   }
   ```
   The sidecar ticket now contains the `Recipient` field which encodes Alice's
   node pubkey and pubkey that'll be used for the channel funding.

   Rationale: Alice will be counter-signing the commitment transactions for the
   new channel so she needs to have the private key for her 2-of-2 multisig
   channel funding key. We derive that from her keychain.

At this point if the `auto` flag was set in the initial ticket, then we're done
here! Both sides now just simply wait for the next batch, to be executed
which'll result in a new sidecar channel being created.

Otherwise, Alice and Charlie will need to carry out another round of
communication:

1. Charlie can now create the bid order with the updated ticket he got from
   Alice:
   ```shell
   charlie$  pool orders submit bid --acct_key <charlie-key> \
     --interest_rate_percent xxx --sidecar_ticket  sidecarAAQgHBgUEAwIBAAMB...
   {
      "order_nonce": "0011223344...",
     "sidecar_ticket": "sidecar15o1Y9oXtyKr3hs2UQho9YmJKbSmB...."
   }
   ```
   The sidecar ticket now contains the order's nonce in the `Order` part.
   Charlie can give the final version of the ticket back to Alice.
2. Alice instructs her node to start expecting a channel.
   ```shell
   alice$  pool sidecar expect-channel sidecarAAQgHBgUEAwIBAAMBYwUBAg...
   ```
