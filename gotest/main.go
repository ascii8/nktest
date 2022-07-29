package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"nhooyr.io/websocket"
)

func main() {
	host := flag.String("host", "127.0.0.1", "url")
	port := flag.Int("port", 8080, "port")
	path := flag.String("path", "/ws", "path")
	key := flag.String("key", "", "key")
	token := flag.String("token", "", "token")
	flag.Parse()
	if err := run(context.Background(), *host, *port, *path, *key, *token); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, host string, port int, path, key, token string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	urlstr := "ws://" + host + ":" + strconv.Itoa(port) + path + "?token=" + token
	log.Printf("url: %s", urlstr)
	// build auth
	var auth string
	if key != "" {
		auth = base64.StdEncoding.EncodeToString([]byte(key + ":"))
	}
	header := http.Header{}
	header.Set("Authorization", "Basic "+auth)
	conn, _, err := websocket.Dial(ctx, urlstr, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
		HTTPHeader:      header,
	})
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "going away")
	return nil
}
