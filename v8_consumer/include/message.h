// Copyright (c) 2017 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an "AS IS"
// BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing
// permissions and limitations under the License.

#ifndef MESSAGE_H
#define MESSAGE_H

#include <memory>
#include <queue>
#include <string>
#include <uv.h>
#include <vector>

typedef std::vector<char> message_buffer;

struct MessageReq {
  uv_write_t request;
};

class Message {
public:
  Message(const std::string &msg);
  uv_buf_t *GetBuf();

protected:
  message_buffer buffer;
  uv_buf_t cached_buffer;
};

class WriteBufPool {
public:
  WriteBufPool();
  uv_write_t *GetNewWriteBuf();
  void Release();

protected:
  std::queue<std::unique_ptr<uv_write_t>> unused_wr_buf_pool;
  std::queue<std::unique_ptr<uv_write_t>> used_wr_buf_pool;
};

class MessagePool {
public:
  std::queue<std::shared_ptr<Message>> messages;
  WriteBufPool write_bufs;
};

#endif
