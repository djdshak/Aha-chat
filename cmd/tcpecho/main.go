package main

import(
	"io"
	"log"
	"net"
)

func main(){
	ln, err := net.Listen("tcp", "127.0.0.1:9090")
	if err != nil {
		log.Fatal(err)
	}

	log.Println("listening on 127.0.0.1:9090")

	for {
		conn, err := ln.Accept()·
		if err != nil {
			log.Println("accept:", err)
			continue;
		}

		log.Println("accepted:", conn.RemoteAddr())

		go func(c net.Conn) {
			defer c.Close()
			_, _ = io.Copy(c, c)
			log.Println("closed:", c.RemoteAddr())
		}(conn)
	}
}