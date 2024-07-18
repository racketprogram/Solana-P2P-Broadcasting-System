package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/blocto/solana-go-sdk/client"
	"github.com/blocto/solana-go-sdk/common"
	"github.com/blocto/solana-go-sdk/rpc"
	"github.com/blocto/solana-go-sdk/types"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/mr-tron/base58"
)

type Node struct {
	ID                string
	Host              host.Host
	Peers             []peer.ID
	Leader            peer.ID
	Round             int
	Keypair           types.Account
	RPCClient         *client.Client
	VotesReceived     map[string]int
	Proposals         map[string]string
	QuorumSize        int
	disconnectedPeers chan peer.ID
}

type WalletInfo struct {
	PublicKey  string    `json:"public_key"`
	PrivateKey string    `json:"private_key"`
	CreatedAt  time.Time `json:"created_at"`
	Network    string    `json:"network"`
}

func NewNode() (*Node, error) {
    priv, pub, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
    if err != nil {
        return nil, fmt.Errorf("failed to generate key pair: %v", err)
    }
    
    pubKeyBytes, _ := pub.Raw()
    pubKeyString := base58.Encode(pubKeyBytes)
    log.Printf("Generated new key pair. Public key: %s", pubKeyString)

    h, err := libp2p.New(
        libp2p.Identity(priv),
        libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"),
        libp2p.Security(noise.ID, noise.New),
        libp2p.Transport(tcp.NewTCPTransport),
        libp2p.NATPortMap(),
    )
    if err != nil {
        return nil, fmt.Errorf("failed to create libp2p host: %v", err)
    }

    log.Printf("Created libp2p host with ID: %s", h.ID())

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
		ID:                uuid.New().String(),
		Host:              h,
		Peers:             make([]peer.ID, 0),
		Keypair:           account,
		RPCClient:         client.NewClient(rpc.DevnetRPCEndpoint),
		VotesReceived:     make(map[string]int),
		Proposals:         make(map[string]string),
		QuorumSize:        0,
		disconnectedPeers: make(chan peer.ID, 100),
	}

    log.Printf("Node created with ID: %s", n.ID)
    log.Printf("Node's libp2p host ID: %s", n.Host.ID())

	return n, nil
}

func (n *Node) DiscoverPeers(ctx context.Context) error {
    serviceName := "solana-p2p"
    log.Printf("Starting mDNS discovery with service name: %s", serviceName)
    
    s := mdns.NewMdnsService(n.Host, serviceName, &discoveryNotifee{node: n})
    return s.Start()
}

type discoveryNotifee struct {
	node *Node
}

func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == n.node.Host.ID() {
		return
	}
    log.Printf("Found peer: %s", pi.ID)
	err := n.node.Host.Connect(context.Background(), pi)
	if err != nil {
		log.Printf("Error connecting to peer %s: %v\n", pi.ID, err)
		return
	}
	n.node.Peers = append(n.node.Peers, pi.ID)
	log.Printf("Connected to peer: %s\n", pi.ID)
}

func (n *Node) ProposeLeader() {
    n.Round++
    if len(n.Peers) > 0 {
        proposedLeader := n.Peers[n.Round%len(n.Peers)]
        proposal := fmt.Sprintf("LEADER:%s:%d\n", proposedLeader.String(), n.Round)
        n.BroadcastMessage(proposal)
        n.HandleProposal(proposal) // 自己也要处理这个提议
    } else {
        n.Leader = n.Host.ID()
        log.Printf("No peers, self-elected as leader for round %d\n", n.Round)
    }
}

func (n *Node) HandleProposal(proposal string) {
    parts := strings.Split(strings.TrimSpace(proposal), ":")
    if len(parts) != 3 || parts[0] != "LEADER" {
        log.Printf("Invalid leader proposal format: %s\n", proposal)
        return
    }

    proposedLeader, err := peer.Decode(parts[1])
    if err != nil {
        log.Printf("Error decoding proposed leader ID: %v\n", err)
        return
    }

    round, err := strconv.Atoi(parts[2])
    if err != nil {
        log.Printf("Error parsing round number: %v\n", err)
        return
    }

    if round < n.Round {
        log.Printf("Received outdated leader proposal for round %d, current round is %d\n", round, n.Round)
        return
    }

    n.Round = round
    vote := fmt.Sprintf("VOTE:LEADER:%s:%d\n", proposedLeader.String(), round)
    n.BroadcastMessage(vote)

    n.VotesReceived[proposal]++
    if n.VotesReceived[proposal] > n.QuorumSize {
        n.Leader = proposedLeader
        log.Printf("New leader elected: %s for round %d\n", n.Leader, n.Round)
    }
}


func (n *Node) BroadcastMessage(message string) int {
	receivedCount := 0
	for _, peerID := range n.Peers {
		stream, err := n.Host.NewStream(context.Background(), peerID, "/solana/1.0.0")
		if err != nil {
			log.Printf("Error creating stream to %s: %v\n", peerID, err)
			continue
		}
		_, err = stream.Write([]byte(message))
		if err != nil {
			log.Printf("Error sending message to %s: %v\n", peerID, err)
		} else {
			receivedCount++
		}
		stream.Close()
	}
	return receivedCount
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

func (n *Node) ProposeTransaction(message string) {
    if n.Leader != n.Host.ID() {
        log.Printf("Only the leader can propose transactions. Current leader: %s\n", n.Leader)
        return
    }

	messageWithLeaderID := fmt.Sprintf("%s (Leader: %s)", message, n.Leader)

    tx, err := n.SignMessage(messageWithLeaderID)
    if err != nil {
        log.Printf("Error signing message: %v\n", err)
        return
    }
    jsonTx, err := json.Marshal(tx)
    if err != nil {
        log.Printf("Error marshaling transaction: %v\n", err)
        return
    }
    proposal := fmt.Sprintf("TX:%s\n", string(jsonTx))
    n.BroadcastMessage(proposal)
    n.HandleTransactionProposal(proposal) // 领导者也要处理自己的提议
}

func (n *Node) HandleTransactionProposal(proposal string) {
    txData := strings.TrimPrefix(strings.TrimSpace(proposal), "TX:")
    
    var tx types.Transaction
    err := json.Unmarshal([]byte(txData), &tx)
    if err != nil {
        log.Printf("Error unmarshaling transaction: %v\n", err)
        return
    }

    vote := fmt.Sprintf("VOTE:TX:%s\n", txData)
    n.BroadcastMessage(vote)

    n.VotesReceived[proposal]++
	log.Println(n.VotesReceived[proposal], n.QuorumSize)
    if n.VotesReceived[proposal] >= n.QuorumSize {
        if n.Leader == n.Host.ID() {
            err = n.RelayMessage(tx)
            if err != nil {
                log.Printf("Error relaying message: %v\n", err)
            }
        } else {
            log.Printf("Transaction proposal reached quorum, waiting for leader to execute")
        }
    }
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

func (n *Node) checkPeerConnection(p peer.ID) {
    if n.Host.Network().Connectedness(p) != network.Connected {
        log.Printf("Peer %s is no longer connected", p)
        n.disconnectedPeers <- p
    }
}

func (n *Node) removePeer(p peer.ID) {
    for i, peer := range n.Peers {
        if peer == p {
            n.Peers = append(n.Peers[:i], n.Peers[i+1:]...)
            break
        }
    }
    n.updateQuorumSize()
    if p == n.Leader {
        log.Println("Leader disconnected. Initiating new leader election.")
        n.Leader = ""
        n.ProposeLeader()
    }
}

func (n *Node) updateQuorumSize() {
    n.QuorumSize = (len(n.Peers) / 2) + 1
    log.Printf("Updated quorum size to %d", n.QuorumSize)
}

func (n *Node) monitorPeers() {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            for _, p := range n.Peers {
                go n.checkPeerConnection(p)
            }
        case p := <-n.disconnectedPeers:
            n.removePeer(p)
        }
    }
}

func (n *Node) Run() error {
	ctx := context.Background()
	err := n.DiscoverPeers(ctx)
	if err != nil {
		return err
	}

	n.Host.SetStreamHandler("/solana/1.0.0", func(s network.Stream) {
		buf := bufio.NewReader(s)
		str, err := buf.ReadString('\n')
		if err != nil {
			log.Printf("Error reading from stream: %v\n", err)
			return
		}
		log.Println(str)
		n.handleIncomingMessage(strings.TrimSpace(str))
	})

    go n.monitorPeers()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			n.updateQuorumSize()
			if n.Leader == n.Host.ID() {
				message := "Hello, Solana!"
				n.ProposeTransaction(message)
			} else if n.Leader == "" {
				n.ProposeLeader()
			}
		}
	}
}

func (n *Node) handleIncomingMessage(message string) {
    if strings.HasPrefix(message, "LEADER:") {
        n.HandleProposal(message)
    } else if strings.HasPrefix(message, "TX:") {
        n.HandleTransactionProposal(message)
    } else if strings.HasPrefix(message, "VOTE:") {
        parts := strings.Split(strings.TrimSpace(message), ":")
        if len(parts) < 3 {
            log.Printf("Invalid vote format: %s\n", message)
            return
        }
        if parts[1] == "LEADER" {
            leaderProposal := fmt.Sprintf("LEADER:%s:%s\n", parts[2], parts[3])
            n.VotesReceived[leaderProposal]++
        } else if parts[1] == "TX" {
            txProposal := fmt.Sprintf("TX:%s\n", strings.Join(parts[2:], ":"))
            n.VotesReceived[txProposal]++
        }
    }
}

func main() {
	node, err := NewNode()
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