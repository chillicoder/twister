run: clean example
	./example
 
include $(GOROOT)/src/Make.inc

TARG=example
DEPS=../../server ../../websocket ../../pprof
GOFILES=\
    chat.go

include $(GOROOT)/src/Make.cmd
