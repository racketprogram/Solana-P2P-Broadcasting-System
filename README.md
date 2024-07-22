# Solana P2P Broadcasting System

This is a P2P broadcasting system built on the Solana blockchain. It uses libp2p for node-to-node communication and records broadcast messages as transactions on Solana.

## Broadcasting Mechanism

1. Node Discovery: Uses DHT to automatically discover other nodes on the internet.
2. Leader Election: The node with the lowest ID is elected as the leader.
3. Message Broadcasting: All nodes can broadcast messages to the network.
4. Transaction Submission: The leader node submits received messages as transactions to the Solana blockchain.
5. Transaction Recording: All nodes record transaction hashes for later queries.

## Setup and Running

To run the program, use the following command:
```bash
go run main.go -wallet /path/to/your/wallet.json
```
Replace `/path/to/your/wallet.json` with the actual path to your Solana wallet JSON file.


## CLI Usage

After starting the program, you'll see a command prompt. Here are the available commands:

1. Broadcast a message:
   Simply type your message and press Enter.
   Example: `When Lambo?`

2. View all recorded transactions:
   Type `transactions` and press Enter.

3. View details of a specific transaction:
   Type `details <transaction_hash>` and press Enter.
   Example: `details 3mo61Vc2BKRxWBYw3n8qJAKbYdf9LhG7667mNFLTw8R5`

4. View current leader:
   Type `leader` and press Enter.

5. View connected peers:
   Type `peers` and press Enter.

6. Exit the program:
   Type `quit` and press Enter.

Note: Ensure your Solana wallet is properly configured and has sufficient SOL to pay for transaction fees.
