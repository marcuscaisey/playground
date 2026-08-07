#!/usr/bin/env sh
cc -std=c17 -Wall -Wextra -pedantic -o main ./*.c && exec ./main
