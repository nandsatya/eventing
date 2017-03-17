# Copyright (c) 2017 Couchbase, Inc.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#     http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an "AS IS"
# BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
# or implied. See the License for the specific language governing
# permissions and limitations under the License.

CXX=g++
CXFLAGS= -DSTANDALONE_BUILD=1 -std=c++11 -ggdb3 -fno-pie -fno-inline # -O3 -Wall

CBDEPS_DIR=/Users/$(USER)/.cbdepscache/
DYLD_LIBRARY_PATH=/Users/$(USER)/.cbdepscache/lib
CMD_DIR=cmd/producer/

LDFLAGS=-luv -L$(CBDEPS_DIR)lib/ -ljemalloc -L$(CBDEPS_DIR)lib/ -lv8 -lcouchbase \
		-headerpad_max_install_names -ll

SOURCES=v8_consumer/src/client.cc v8_consumer/src/commands.cc \
				v8_consumer/src/message.cc v8_consumer/src/v8worker.cc \
				v8_consumer/src/n1ql.cc v8_consumer/src/bucket.cc \
				v8_consumer/src/parse_deployment.cc \
				v8_consumer/src/log.cc \
				v8_consumer/src/jsify.cc \
				v8_consumer/src/transpiler.cc

INCLUDE_DIRS=-I$(CBDEPS_DIR) -I$(CBDEPS_DIR)include -I v8_consumer/include/

OUT=$(CMD_DIR)client.bin
EVENTING_EXEC=eventing

build: jsify
	$(CBDEPS_DIR)bin/flatc -o flatbuf/include/ -c flatbuf/schema/*.fbs
	$(CBDEPS_DIR)bin/flatc -g flatbuf/schema/*.fbs
	$(CXX) $(CXFLAGS) $(SOURCES) $(INCLUDE_DIRS) $(LDFLAGS) -o $(OUT)
	cd $(CMD_DIR); go build -o $(EVENTING_EXEC); bash fix_rpath.sh

jsify: v8_consumer/src/jsify.lex
	$(LEX) -o v8_consumer/src/jsify.cc v8_consumer/src/jsify.lex
	sed -i "" 's/register//' v8_consumer/src/jsify.cc

allopt:
	$(CXX) $(CXFLAGS) $(SOURCES) $(INCLUDE_DIRS) $(LDFLAGS) -O3 -o $(OUT)
	cd $(CMD_DIR); go build; bash fix_rpath.sh
	cd $(CMD_DIR); ./$(EVENTING_EXEC)

run: build
	cd $(CMD_DIR); ./$(EVENTING_EXEC)

clean:
	-rm -rf $(OUT) $(CMD_DIR)$(EVENTING_EXEC)
	-rm -rf flatbuf/cfg/ flatbuf/header/ flatbuf/include/ flatbuf/payload/
	-rm v8_consumer/src/jsify.cc