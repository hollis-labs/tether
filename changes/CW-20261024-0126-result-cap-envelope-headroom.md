- Reserve 256 bytes of the result cap for the JSON-RPC envelope (CW-20261024-0126). The cap measured the tool result alone, so a page sized
  exactly to it exceeded the cap on the wire by up to about 130 bytes. First pages and `tether_result_next` pages now fit the cap on the whole response
  line (test measures a full JSON-RPC line and fails without the reserve). Documented that a single item larger than the cap is delivered whole.
