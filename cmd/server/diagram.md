客户端A --(WS发消息)--> A.readPump
                         |
                         v
                    hub.broadcast  (所有消息进入这里)
                         |
                         v
                      hub.run
               (分发到所有在线连接)
              /        |        \
             v         v         v
          A.send     B.send     C.send
             |         |         |
             v         v         v
        A.writePump B.writePump C.writePump
             |         |         |
             v         v         v
         写回A       写回B       写回C
