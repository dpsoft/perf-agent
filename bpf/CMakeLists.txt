cmake_minimum_required(VERSION 4.0)
project(bpf C)

set(CMAKE_C_STANDARD 11)

include_directories(.)

add_executable(bpf
        cpu.bpf.c
        perf.bpf.c
        vmlinux.h)
