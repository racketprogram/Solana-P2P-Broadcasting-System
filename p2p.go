package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/blocto/solana-go-sdk/client"
	"github.com/blocto/solana-go-sdk/common"
	"github.com/blocto/solana-go-sdk/rpc"
	"github.com/blocto/solana-go-sdk/types"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

type Node struct {
	ID        string
	Host      host.Host
	Peers     []peer.ID
	Leader    peer.ID
	Round     int
	Keypair   types.Account
	RPCClient *client.Client
}

type WalletInfo struct {
    PublicKey  string    `json:"public_key"`
    PrivateKey string    `json:"private_key"`
    CreatedAt  time.Time `json:"created_at"`
    Network    string    `json:"network"`
}

func NewNode(id string) (*Node, error) {
	h, err := libp2p.New()
	if err != nil {
		return nil, err
	}

	jsonFile, err := os.ReadFile("./solana_wallet_20240718_111115.json")
	if err != nil {
		return nil, fmt.Errorf("error reading wallet file: %v", err)
	}

	var walletInfo WalletInfo
	if err := json.Unmarshal(jsonFile, &walletInfo); err != nil {
		return nil, fmt.Errorf("error parsing wallet JSON: %v", err)
	}

	account, err := types.AccountFromBase58(walletInfo.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %v", err)
	}

	n := &Node{
		ID:        id,
		Host:      h,
		Peers:     make([]peer.ID, 0),
		Keypair:   account,
		RPCClient: client.NewClient(rpc.DevnetRPCEndpoint),
	}

	return n, nil
}

func (n *Node) DiscoverPeers(ctx context.Context) error {
	s := mdns.NewMdnsService(n.Host, "solana-p2p-jimmy", &discoveryNotifee{node: n})
	return s.Start()
}

type discoveryNotifee struct {
	node *Node
}

func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == n.node.Host.ID() {
		return
	}
	err := n.node.Host.Connect(context.Background(), pi)
	if err != nil {
		log.Printf("Error connecting to peer %s: %v\n", pi.ID, err)
		return
	}
	n.node.Peers = append(n.node.Peers, pi.ID)
	log.Printf("Connected to peer: %s\n", pi.ID)
}

func (n *Node) SelectLeader() {
	n.Round++
	if len(n.Peers) > 0 { 
		n.Leader = n.Peers[n.Round%len(n.Peers)]
	} else {
		n.Leader = n.Host.ID()
	}
	log.Printf("Leader for round %d: %s\n", n.Round, n.Leader)
}

func (n *Node) BroadcastMessage(message string) error {
	for _, peerID := range n.Peers {
		stream, err := n.Host.NewStream(context.Background(), peerID, "/solana/1.0.0")
		if err != nil {
			log.Printf("Error creating stream to %s: %v\n", peerID, err)
			continue
		}
		_, err = stream.Write([]byte(message))
		if err != nil {
			log.Printf("Error sending message to %s: %v\n", peerID, err)
		}
		stream.Close()
	}
	return nil
}

func (n *Node) SignMessage(message string) (types.Transaction, error) {
	recentBlockhash, err := n.RPCClient.GetLatestBlockhash(context.Background())
	if err != nil {
		return types.Transaction{}, err
	}

	tx, err := types.NewTransaction(types.NewTransactionParam{
		Message: types.NewMessage(types.NewMessageParam{
			FeePayer:        n.Keypair.PublicKey,
			RecentBlockhash: recentBlockhash.Blockhash,
			Instructions: []types.Instruction{
				{
					ProgramID: common.MemoProgramID,
					Accounts:  []types.AccountMeta{},
					Data:      []byte(message),
				},
			},
		}),
		Signers: []types.Account{n.Keypair},
	})
	if err != nil {
		return types.Transaction{}, err
	}

	return tx, nil
}

func (n *Node) RelayMessage(tx types.Transaction) error {
	balance, err := n.RPCClient.GetBalance(context.Background(), n.Keypair.PublicKey.ToBase58())
	if err != nil {
		log.Printf("Error getting balance: %v\n", err)
		return err
	}
	log.Printf("Current balance: %d lamports (%.5f SOL)\n", balance, float64(balance)/1e9)

	if balance == 0 {
		log.Println("Account has no balance. Please fund it using Devnet Faucet.")
		return fmt.Errorf("insufficient balance")
	}

	sig, err := n.RPCClient.SendTransaction(context.Background(), tx)
	if err != nil {
		log.Printf("Error sending transaction: %v\n", err)
		return err
	}
	log.Printf("Transaction sent: %s\n", sig)
	return nil
}

func (n *Node) Run() error {
	ctx := context.Background()
	err := n.DiscoverPeers(ctx)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			n.SelectLeader()
			if n.Leader == n.Host.ID() {
				message := "Hello, Solana!"
				err := n.BroadcastMessage(message)
				if err != nil {
					log.Printf("Error broadcasting message: %v\n", err)
				}
				signedTx, err := n.SignMessage(message)
				if err != nil {
					log.Printf("Error signing message: %v\n", err)
				} else {
					err = n.RelayMessage(signedTx)
					if err != nil {
						log.Printf("Error relaying message: %v\n", err)
					}
				}
			}
		}
	}
}

func main() {
	node, err := NewNode("node0")
	if err != nil {
		log.Fatalf("Error creating node: %v\n", err)
	}

	fmt.Printf("Node is running with ID: %s\n", node.Host.ID())
	fmt.Printf("Solana Public Key: %s\n", node.Keypair.PublicKey.ToBase58())

	go func() {
		if err := node.Run(); err != nil {
			log.Fatalf("Error running node: %v\n", err)
		}
	}()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Enter command (peers, leader, balance, quit): ")
		command, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println("Error reading input:", err)
			continue
		}
		command = strings.TrimSpace(command)
		switch command {
		case "peers":
			fmt.Println("Connected peers:", node.Peers)
		case "leader":
			fmt.Println("Current leader:", node.Leader)
		case "balance":
			balance, err := node.RPCClient.GetBalance(context.Background(), node.Keypair.PublicKey.ToBase58())
			if err != nil {
				fmt.Printf("Error getting balance: %v\n", err)
			} else {
				fmt.Printf("Current balance: %d lamports (%.5f SOL)\n", balance, float64(balance)/1e9)
			}
		case "quit":
			fmt.Println("Quitting...")
			return
		default:
			fmt.Println("Unknown command")
		}
	}
}
