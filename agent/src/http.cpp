#include <arpa/inet.h>
#include <netdb.h>
#include <netinet/in.h>
#include <poll.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

#include <cerrno>
#include <cstring>

#include "chipagent.hpp"

namespace chipagent {
namespace {

constexpr size_t kMaxRequestBytes = 8192;
constexpr int kIoTimeoutSeconds = 2;

#ifdef MSG_NOSIGNAL
constexpr int kSendFlags = MSG_NOSIGNAL;
#else
constexpr int kSendFlags = 0;  // macOS: SO_NOSIGPIPE is set per socket instead
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

void SendAll(int fd, const std::string& data) {
  size_t off = 0;
  while (off < data.size()) {
    ssize_t n = send(fd, data.data() + off, data.size() - off, kSendFlags);
    if (n <= 0) return;
    off += static_cast<size_t>(n);
  }
}

void Write(int fd, const HttpServer::Response& r) {
  std::string head = "HTTP/1.1 " + std::to_string(r.status) + " " + Reason(r.status) +
                     "\r\nContent-Type: " + r.content_type +
                     "\r\nContent-Length: " + std::to_string(r.body.size()) +
                     "\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n";
  SendAll(fd, head + r.body);
}

}  // namespace

HttpServer::~HttpServer() {
  if (fd_ >= 0) close(fd_);
}

bool HttpServer::Listen(const std::string& addr, std::string* err) {
  size_t colon = addr.rfind(':');
  if (colon == std::string::npos) {
    *err = "listen address must be host:port, got \"" + addr + "\"";
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
    int fd = socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
    if (fd < 0) continue;
    int one = 1;
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
    if (bind(fd, ai->ai_addr, ai->ai_addrlen) == 0 && listen(fd, 64) == 0) {
      fd_ = fd;
      break;
    }
    *err = "listen on " + addr + ": " + std::strerror(errno);
    close(fd);
  }
  freeaddrinfo(res);
  if (fd_ < 0) return false;

  sockaddr_storage ss{};
  socklen_t len = sizeof ss;
  getsockname(fd_, reinterpret_cast<sockaddr*>(&ss), &len);
  port_ = ss.ss_family == AF_INET6 ? ntohs(reinterpret_cast<sockaddr_in6*>(&ss)->sin6_port)
                                   : ntohs(reinterpret_cast<sockaddr_in*>(&ss)->sin_port);
  return true;
}

void HttpServer::Serve(const Handler& handler, const std::atomic<bool>& stop) {
  while (!stop) {
    pollfd p{fd_, POLLIN, 0};
    if (poll(&p, 1, 200) <= 0) continue;  // timeout or signal: re-check stop
    int c = accept(fd_, nullptr, nullptr);
    if (c < 0) continue;
    HandleConnection(c, handler);
    close(c);
  }
}

void HttpServer::HandleConnection(int fd, const Handler& handler) {
  timeval tv{kIoTimeoutSeconds, 0};
  setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
  setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof tv);
#ifdef SO_NOSIGPIPE
  int one = 1;
  setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, sizeof one);
#endif

  std::string req;
  char buf[2048];
  while (req.find("\r\n\r\n") == std::string::npos) {
    if (req.size() >= kMaxRequestBytes) return Write(fd, {431, "text/plain", "request too large\n"});
    ssize_t n = recv(fd, buf, sizeof buf, 0);
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
