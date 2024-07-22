package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/blocto/solana-go-sdk/client"
	"github.com/blocto/solana-go-sdk/common"
	"github.com/blocto/solana-go-sdk/rpc"
	"github.com/blocto/solana-go-sdk/types"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/mr-tron/base58"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
)

var defaultBootstrapPeers = []string{
	"/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",
}

type Node struct {
	Host              host.Host
	Peers             map[peer.ID]bool
	Leader            peer.ID
	Keypair           types.Account
	RPCClient         *client.Client
	mutex             sync.Mutex
	ctx               context.Context
	cancelFunc        context.CancelFunc
	TransactionHashes []string
	dht               *dht.IpfsDHT
}

func NewNode(account types.Account) (*Node, error) {
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		return nil, fmt.Errorf("failed to generate key pair: %v", err)
	}

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

	bootstrapPeers, err := convertPeers(defaultBootstrapPeers)
	if err != nil {
		return nil, fmt.Errorf("failed to convert bootstrap peers: %v", err)
	}

	dhtOpts := []dht.Option{
		dht.Mode(dht.ModeAutoServer),
		dht.BootstrapPeers(bootstrapPeers...),
	}
	kadDHT, err := dht.New(context.Background(), h, dhtOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create DHT: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	n := &Node{
		Host:       h,
		Peers:      make(map[peer.ID]bool),
		Keypair:    account,
		RPCClient:  client.NewClient(rpc.DevnetRPCEndpoint),
		ctx:        ctx,
		cancelFunc: cancel,
		dht:        kadDHT,
	}

	return n, nil
}

func (n *Node) Cleanup() error {
	ctx, cancel := context.WithTimeout(n.ctx, 30*time.Second)
	defer cancel()

	err := n.dht.Provide(ctx, createSolanaPeerCID(), false)
	if err != nil {
		log.Printf("Error stopping providing service: %v", err)
	}

	peers := n.GetPeers()
	for _, peer := range peers {
		if err := n.Host.Network().ClosePeer(peer); err != nil {
			log.Printf("Error closing connection to peer %s: %v", peer.String(), err)
		}
	}

	err = n.Host.Close()
	if err != nil {
		return fmt.Errorf("error closing libp2p host: %v", err)
	}

	n.cancelFunc()

	return nil
}

func convertPeers(peers []string) ([]peer.AddrInfo, error) {
	var pinfos []peer.AddrInfo
	for _, addr := range peers {
		maddr, err := multiaddr.NewMultiaddr(addr)
		if err != nil {
			return nil, err
		}
		p, err := peer.AddrInfoFromP2pAddr(maddr)
		if err != nil {
			return nil, err
		}
		pinfos = append(pinfos, *p)
	}
	return pinfos, nil
}

func (n *Node) DiscoverPeers() error {
	if err := n.dht.Bootstrap(n.ctx); err != nil {
		return fmt.Errorf("error bootstrapping DHT: %v", err)
	}

	go n.findPeers()

	return nil
}

func createSolanaPeerCID() cid.Cid {
	h, _ := multihash.Sum([]byte("solana-p2p-345346534634"), multihash.SHA2_256, -1)
	return cid.NewCidV1(cid.Raw, h)
}

func (n *Node) findPeers() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			if err := n.dht.Provide(n.ctx, createSolanaPeerCID(), true); err != nil {
				log.Printf("Error providing: %v\n", err)
			}

			providers := n.dht.FindProvidersAsync(n.ctx, createSolanaPeerCID(), 20)
			for p := range providers {
				if p.ID == n.Host.ID() {
					continue
				}
				n.connectToPeer(p)
			}
		}
	}
}

func (n *Node) connectToPeer(peer peer.AddrInfo) {
	if peer.ID == n.Host.ID() {
		return
	}

	n.mutex.Lock()
	_, exists := n.Peers[peer.ID]
	n.mutex.Unlock()
	if exists {
		return
	}

	if err := n.Host.Connect(n.ctx, peer); err != nil {
		log.Printf("Error connecting to peer %s: %v\n", peer.ID, err)
	} else {
		n.mutex.Lock()
		n.Peers[peer.ID] = true
		n.mutex.Unlock()
		log.Printf("Connected to peer: %s\n", peer.ID)
		n.ElectLeader()
	}
}

func (n *Node) GetPeers() []peer.ID {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	peers := make([]peer.ID, 0, len(n.Peers))
	for peerID := range n.Peers {
		peers = append(peers, peerID)
	}
	return peers
}

func (n *Node) ElectLeader() {
	if len(n.Peers) == 0 {
		n.Leader = n.Host.ID()
		log.Printf("No peers, self-elected as leader")
		return
	}

	peers := n.GetPeers()
	lowestID := n.Host.ID()
	for _, peerID := range peers {
		if peerID < lowestID {
			lowestID = peerID
		}
	}

	n.Leader = lowestID
	log.Printf("New leader elected: %s\n", n.Leader)
}

func (n *Node) BroadcastMessage(message string) {
	peers := n.GetPeers()

	for _, peerID := range peers {
		stream, err := n.Host.NewStream(n.ctx, peerID, "/solana/1.0.0")
		if err != nil {
			log.Printf("Error creating stream to %s: %v\n", peerID, err)
			continue
		}
		_, err = stream.Write([]byte(message + "\n"))
		if err != nil {
			log.Printf("Error sending message to %s: %v\n", peerID, err)
		}
		stream.Close()
	}
}

func (n *Node) BroadcastTransactionHash(hash string) {
	message := fmt.Sprintf("TXHASH:%s", hash)
	n.BroadcastMessage(message)
}

func (n *Node) RecordTransactionHash(hash string) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	n.TransactionHashes = append(n.TransactionHashes, hash)
	log.Printf("Recorded transaction hash: %s\n", hash)
}

func (n *Node) GetAllTransactionHashes() []string {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	return append([]string{}, n.TransactionHashes...)
}

func (n *Node) GetTransactionDetails(hash string) (string, error) {
	tx, err := n.RPCClient.GetTransaction(n.ctx, hash)
	if err != nil {
		return "", fmt.Errorf("error fetching transaction: %v", err)
	}

	details := fmt.Sprintf("Transaction: %s\n", hash)
	details += fmt.Sprintf("Slot: %d\n", tx.Slot)
	details += fmt.Sprintf("Block Time: %d\n", *tx.BlockTime)

	details += "Accounts:\n"
	for i, acc := range tx.Transaction.Message.Accounts {
		details += fmt.Sprintf("  %d. %s\n", i+1, acc.ToBase58())
	}

	details += "Signers:\n"
	for i, sig := range tx.Transaction.Signatures {
		details += fmt.Sprintf("  %d. %s\n", i+1, tx.Transaction.Message.Accounts[i].ToBase58())
		details += fmt.Sprintf("     Signature: %s\n", base58.Encode(sig[:]))
	}

	for i, inst := range tx.Transaction.Message.Instructions {
		details += fmt.Sprintf("Instruction %d:\n", i+1)
		details += fmt.Sprintf("  Program ID: %s\n", tx.Transaction.Message.Accounts[inst.ProgramIDIndex].ToBase58())
		details += fmt.Sprintf("  Data: %s\n", inst.Data)
	}

	return details, nil
}

func (n *Node) SendMessageAsTransaction(message string) (string, error) {
	recentBlockhash, err := n.RPCClient.GetLatestBlockhash(n.ctx)
	if err != nil {
		return "", err
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
		return "", err
	}

	sig, err := n.RPCClient.SendTransaction(n.ctx, tx)
	if err != nil {
		return "", err
	}
	log.Printf("Transaction sent: %s\n", sig)
	return sig, nil
}

func (n *Node) Run() error {
	err := n.DiscoverPeers()
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
		message := strings.TrimSpace(str)

		if strings.HasPrefix(message, "TXHASH:") {
			hash := strings.TrimPrefix(message, "TXHASH:")
			n.RecordTransactionHash(hash)
		} else {
			log.Printf("Received message: %s\n", message)
			if n.Leader == n.Host.ID() {
				hash, err := n.SendMessageAsTransaction(message)
				if err != nil {
					log.Printf("Error sending transaction: %v\n", err)
				} else {
					n.RecordTransactionHash(hash)
					n.BroadcastTransactionHash(hash)
				}
			}
		}
	})

	go n.monitorPeers()

	return nil
}

func (n *Node) monitorPeers() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			n.checkPeersConnection()
		}
	}
}

func (n *Node) checkPeersConnection() {
	for peerID := range n.Peers {
		if n.Host.Network().Connectedness(peerID) != network.Connected {
			log.Printf("Peer %s disconnected\n", peerID)
			n.mutex.Lock()
			delete(n.Peers, peerID)
			n.mutex.Unlock()
			if peerID == n.Leader {
				n.ElectLeader()
			}
		}
	}
}

func loadWallet(filepath string) (types.Account, error) {
	jsonFile, err := os.ReadFile(filepath)
	if err != nil {
		return types.Account{}, fmt.Errorf("error reading wallet file: %v", err)
	}

	var walletInfo struct {
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal(jsonFile, &walletInfo); err != nil {
		return types.Account{}, fmt.Errorf("error parsing wallet JSON: %v", err)
	}

	account, err := types.AccountFromBase58(walletInfo.PrivateKey)
	if err != nil {
		return types.Account{}, fmt.Errorf("invalid private key: %v", err)
	}

	return account, nil
}

func main() {
	walletPath := flag.String("wallet", "", "Path to the wallet JSON file")
	flag.Parse()

	if *walletPath == "" {
		log.Fatalf("Please specify the wallet JSON file path using -wallet flag")
	}

	account, err := loadWallet(*walletPath)
	if err != nil {
		log.Fatalf("Error loading wallet: %v", err)
	}

	node, err := NewNode(account)
	if err != nil {
		log.Fatalf("Error creating node: %v\n", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Printf("Node is running with ID: %s\n", node.Host.ID())
	fmt.Printf("Solana Public Key: %s\n", node.Keypair.PublicKey.ToBase58())

	go func() {
		if err := node.Run(); err != nil {
			log.Fatalf("Error running node: %v\n", err)
		}
	}()

	node.ElectLeader()

	go func() {

		reader := bufio.NewReader(os.Stdin)
		for {
			fmt.Print("Enter message (or 'quit' to exit): ")
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)

			switch {
			case input == "quit":
				node.cancelFunc()
				sigChan <- syscall.SIGTERM
				return
			case input == "transactions":
				hashes := node.GetAllTransactionHashes()
				if len(hashes) == 0 {
					fmt.Println("No transactions recorded yet.")
				} else {
					fmt.Println("Recorded transactions:")
					for i, hash := range hashes {
						fmt.Printf("%d. %s\n", i+1, hash)
					}
				}
			case strings.HasPrefix(input, "details "):
				hash := strings.TrimPrefix(input, "details ")
				details, err := node.GetTransactionDetails(hash)
				if err != nil {
					fmt.Printf("error getting transaction details: %v", err)
				}
				fmt.Println(details)
			case input == "leader":
				fmt.Printf("Current leader: %s\n", node.Leader)
				if node.Leader == node.Host.ID() {
					fmt.Println("This node is the current leader.")
				}
			case input == "peers":
				peers := node.GetPeers()
				if len(peers) == 0 {
					fmt.Println("No peers connected.")
				} else {
					fmt.Println("Connected peers:")
					for i, peer := range peers {
						fmt.Printf("%d. %s\n", i+1, peer)
					}
				}
			default:
				if node.Leader == node.Host.ID() {
					hash, err := node.SendMessageAsTransaction(input)
					if err != nil {
						fmt.Printf("Error sending transaction: %v\n", err)
					} else {
						node.RecordTransactionHash(hash)
						node.BroadcastTransactionHash(hash)
					}
				}
				node.BroadcastMessage(input)
			}
		}
	}()

	<-sigChan
	fmt.Println("Received exit signal. Starting cleanup...")

	if err := node.Cleanup(); err != nil {
		log.Printf("Error during cleanup: %v", err)
	} else {
		fmt.Println("Cleanup completed successfully.")
	}

	time.Sleep(2 * time.Second)
	fmt.Println("Exiting program.")
}
