#include "chipagent.hpp"

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#else
#include <arpa/inet.h>
#include <netdb.h>
#include <netinet/in.h>
#include <poll.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>
#endif

#include <cerrno>
#include <cstring>

namespace chipagent {
namespace {

// A thin layer over the differences between BSD sockets and Winsock.
#ifdef _WIN32
using Sock = SOCKET;
constexpr std::intptr_t kNoSocket = -1;
Sock AsSock(std::intptr_t fd) { return static_cast<Sock>(fd); }
void CloseSocket(std::intptr_t fd) { closesocket(AsSock(fd)); }
int PollOne(std::intptr_t fd, int timeout_ms) {
  WSAPOLLFD p{AsSock(fd), POLLRDNORM, 0};
  return WSAPoll(&p, 1, timeout_ms);
}
void SetTimeouts(std::intptr_t fd, int seconds) {
  DWORD ms = static_cast<DWORD>(seconds) * 1000;
  setsockopt(AsSock(fd), SOL_SOCKET, SO_RCVTIMEO, reinterpret_cast<const char*>(&ms), sizeof ms);
  setsockopt(AsSock(fd), SOL_SOCKET, SO_SNDTIMEO, reinterpret_cast<const char*>(&ms), sizeof ms);
}
std::string LastSocketError() { return "winsock error " + std::to_string(WSAGetLastError()); }
bool StartSockets() {
  static const bool ok = [] {
    WSADATA wsa;
    return WSAStartup(MAKEWORD(2, 2), &wsa) == 0;
  }();
  return ok;
}
#else
using Sock = int;
constexpr std::intptr_t kNoSocket = -1;
Sock AsSock(std::intptr_t fd) { return static_cast<Sock>(fd); }
void CloseSocket(std::intptr_t fd) { close(AsSock(fd)); }
int PollOne(std::intptr_t fd, int timeout_ms) {
  pollfd p{AsSock(fd), POLLIN, 0};
  return poll(&p, 1, timeout_ms);
}
void SetTimeouts(std::intptr_t fd, int seconds) {
  timeval tv{seconds, 0};
  setsockopt(AsSock(fd), SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
  setsockopt(AsSock(fd), SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof tv);
#ifdef SO_NOSIGPIPE
  int one = 1;
  setsockopt(AsSock(fd), SOL_SOCKET, SO_NOSIGPIPE, &one, sizeof one);
#endif
}
std::string LastSocketError() { return std::strerror(errno); }
bool StartSockets() { return true; }
#endif

constexpr size_t kMaxRequestBytes = 8192;
constexpr int kIoTimeoutSeconds = 2;

#ifdef MSG_NOSIGNAL
constexpr int kSendFlags = MSG_NOSIGNAL;
#else
constexpr int kSendFlags = 0;  // macOS sets SO_NOSIGPIPE per socket; Windows has no SIGPIPE
#endif

const char* Reason(int status) {
  switch (status) {
    case 200: return "OK";
    case 400: return "Bad Request";
    case 404: return "Not Found";
    case 405: return "Method Not Allowed";
    case 431: return "Request Header Fields Too Large";
  }
  return "Internal Server Error";
}

void SendAll(std::intptr_t fd, const std::string& data) {
  size_t off = 0;
  while (off < data.size()) {
    auto n = send(AsSock(fd), data.data() + off, static_cast<int>(data.size() - off), kSendFlags);
    if (n <= 0) return;
    off += static_cast<size_t>(n);
  }
}

void Write(std::intptr_t fd, const HttpServer::Response& r) {
  std::string head = "HTTP/1.1 " + std::to_string(r.status) + " " + Reason(r.status) +
                     "\r\nContent-Type: " + r.content_type +
                     "\r\nContent-Length: " + std::to_string(r.body.size()) +
                     "\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n";
  SendAll(fd, head + r.body);
}

}  // namespace

HttpServer::~HttpServer() {
  if (fd_ != kNoSocket) CloseSocket(fd_);
}

bool HttpServer::Listen(const std::string& addr, std::string* err) {
  size_t colon = addr.rfind(':');
  if (colon == std::string::npos) {
    *err = "listen address must be host:port, got \"" + addr + "\"";
    return false;
  }
  if (!StartSockets()) {
    *err = "cannot initialize sockets";
    return false;
  }
  std::string host = addr.substr(0, colon), port = addr.substr(colon + 1);
  if (host.size() >= 2 && host.front() == '[' && host.back() == ']') host = host.substr(1, host.size() - 2);

  addrinfo hints{};
  hints.ai_family = AF_UNSPEC;
  hints.ai_socktype = SOCK_STREAM;
  hints.ai_flags = AI_PASSIVE;
  addrinfo* res = nullptr;
  if (int rc = getaddrinfo(host.empty() ? nullptr : host.c_str(), port.c_str(), &hints, &res); rc != 0) {
    *err = "resolve " + addr + ": " + gai_strerror(rc);
    return false;
  }
  *err = "no usable address for " + addr;
  for (addrinfo* ai = res; ai; ai = ai->ai_next) {
    Sock s = socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
    std::intptr_t fd = static_cast<std::intptr_t>(s);
#ifdef _WIN32
    if (s == INVALID_SOCKET) continue;
#else
    if (s < 0) continue;
    int one = 1;  // Windows SO_REUSEADDR would allow port theft, so only set it on POSIX
    setsockopt(s, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
#endif
    if (bind(s, ai->ai_addr, static_cast<int>(ai->ai_addrlen)) == 0 && listen(s, 64) == 0) {
      fd_ = fd;
      break;
    }
    *err = "listen on " + addr + ": " + LastSocketError();
    CloseSocket(fd);
  }
  freeaddrinfo(res);
  if (fd_ == kNoSocket) return false;

  sockaddr_storage ss{};
  socklen_t len = sizeof ss;
  getsockname(AsSock(fd_), reinterpret_cast<sockaddr*>(&ss), &len);
  port_ = ss.ss_family == AF_INET6 ? ntohs(reinterpret_cast<sockaddr_in6*>(&ss)->sin6_port)
                                   : ntohs(reinterpret_cast<sockaddr_in*>(&ss)->sin_port);
  return true;
}

void HttpServer::Serve(const Handler& handler, const std::atomic<bool>& stop) {
  while (!stop) {
    if (PollOne(fd_, 200) <= 0) continue;  // timeout or signal: re-check stop
    Sock c = accept(AsSock(fd_), nullptr, nullptr);
#ifdef _WIN32
    if (c == INVALID_SOCKET) continue;
#else
    if (c < 0) continue;
#endif
    HandleConnection(static_cast<std::intptr_t>(c), handler);
    CloseSocket(static_cast<std::intptr_t>(c));
  }
}

void HttpServer::HandleConnection(std::intptr_t fd, const Handler& handler) {
  SetTimeouts(fd, kIoTimeoutSeconds);

  std::string req;
  char buf[2048];
  while (req.find("\r\n\r\n") == std::string::npos) {
    if (req.size() >= kMaxRequestBytes) return Write(fd, {431, "text/plain", "request too large\n"});
    auto n = recv(AsSock(fd), buf, static_cast<int>(sizeof buf), 0);
    if (n <= 0) return;  // closed or timed out
    req.append(buf, static_cast<size_t>(n));
  }

  // Request line: METHOD SP target SP HTTP-version
  size_t eol = req.find("\r\n");
  std::string line = req.substr(0, eol);
  size_t a = line.find(' '), b = line.rfind(' ');
  if (a == std::string::npos || a == b) return Write(fd, {400, "text/plain", "bad request\n"});
  std::string method = line.substr(0, a);
  std::string path = line.substr(a + 1, b - a - 1);
  if (size_t q = path.find('?'); q != std::string::npos) path.resize(q);
  Write(fd, handler(method, path));
}

}  // namespace chipagent
