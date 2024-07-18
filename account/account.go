package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"time"

	"github.com/blocto/solana-go-sdk/types"
	"github.com/mr-tron/base58"
)

type WalletInfo struct {
	PublicKey  string    `json:"public_key"`
	PrivateKey string    `json:"private_key"`
	CreatedAt  time.Time `json:"created_at"`
	Network    string    `json:"network"`
}

func main() {
	// 創建新的 Solana 帳戶
	account := types.NewAccount()

	// 將私鑰轉換為 Base58 格式
	privateKeyBase58 := base58.Encode(account.PrivateKey)

	// 準備錢包信息
	walletInfo := WalletInfo{
		PublicKey:  account.PublicKey.ToBase58(),
		PrivateKey: privateKeyBase58,
		CreatedAt:  time.Now(),
		Network:    "devnet", // 指定網絡為 devnet
	}

	// 將錢包信息轉換為 JSON
	jsonData, err := json.MarshalIndent(walletInfo, "", "  ")
	if err != nil {
		log.Fatalf("Error marshaling wallet info: %v", err)
	}

	// 生成文件名
	filename := fmt.Sprintf("solana_wallet_%s.json", time.Now().Format("20060102_150405"))

	// 將 JSON 數據寫入文件
	err = ioutil.WriteFile(filename, jsonData, 0600)
	if err != nil {
		log.Fatalf("Error writing wallet info to file: %v", err)
	}

	fmt.Printf("Wallet created successfully!\n")
	fmt.Printf("Public Key: %s\n", walletInfo.PublicKey)
	fmt.Printf("Wallet information saved to: %s\n", filename)
	fmt.Println("IMPORTANT: Keep your private key safe and never share it with anyone!")
	fmt.Println("This wallet can be used on Solana Devnet for testing and development.")
	fmt.Println("To get test SOL, visit: https://solfaucet.com/")
}