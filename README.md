Inspired by [QUIC](https://www.chromium.org/quic) which is a UDP-based secure and reliable transport for HTTP/2, 
I try to tunnel TCP payload which is received from local socks 5 server over UDP reliably and secrectly. 
So it can be used for bypassing firewalls whcih restrict TCP connection. 
As UDP is connectionless, change of network environment(for example, from wifi to mobile data) can be gracefully 
handled, and enables applications to rapidly resume sessions. (not done yet)
