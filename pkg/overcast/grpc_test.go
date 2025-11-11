package overcast

import (
	"context"
	fmt "fmt"
	"io"
	"testing"

	"github.com/gagliardetto/solana-go"
	"google.golang.org/grpc"
)

func Test_OvercastGrpc(f *testing.T) {
	conn, err := grpc.NewClient("127.0.0.1:13370", grpc.WithInsecure())
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	client := NewSlotStreamClient(conn)
	stream, err := client.StreamSlots(context.Background(), &SlotStreamRequest{})
	if err != nil {
		panic(err)
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return
		} else if err == nil {
			fmt.Printf("slot received: %d, %d entries.", resp.Slot, len(resp.Entries))

			if len(resp.Entries) != 0 {
				if len(resp.GetEntries()[0].GetTransactions()) != 0 {
					tx := resp.GetEntries()[0].GetTransactions()[0]
					var msgLegacy = tx.GetMessageLegacy()
					var msgV0 = tx.GetMessageV0()
					signature := tx.Signatures[0]

					var recentBlockhash []byte

					if msgV0 == nil {
						recentBlockhash = msgLegacy.RecentBlockhash
					} else if msgLegacy == nil {
						recentBlockhash = msgV0.RecentBlockhash
					} else {
						panic("both msgLegacy and msgV0 were nil")
					}
					fmt.Printf("\t\ttx = %s, recent_blockhash = %s\n", solana.SignatureFromBytes(signature), solana.HashFromBytes(recentBlockhash))
				}

				for _, entry := range resp.Entries {
					for _, tx := range entry.Transactions {
						var msgLegacy = tx.GetMessageLegacy()
						var msgV0 = tx.GetMessageV0()
						var blockhash []byte

						if msgV0 == nil {
							blockhash = msgLegacy.RecentBlockhash
						} else if msgLegacy == nil {
							blockhash = msgV0.RecentBlockhash
						} else {
							panic("both msgLegacy and msgV0 were nil")
						}
						fmt.Printf("%s, blockhash = %s\n", solana.SignatureFromBytes(tx.Signatures[0]), solana.HashFromBytes(blockhash))
					}
				}
				fmt.Printf("\n\n")
			}

			relevantEntry := resp.Entries[len(resp.Entries)-1]
			firstEntry := resp.Entries[0]
			fmt.Printf("blockhash for slot %d = %s?\n", resp.Slot, solana.HashFromBytes(relevantEntry.Hash))
			fmt.Printf("first blockhash for slot %d = %s?\n", resp.Slot, solana.HashFromBytes(firstEntry.Hash))
		}

		if err != nil {
			panic(err) // dont use panic in your real project
		}
	}
}
