#!/usr/bin/env sh
rustc --edition=2024 -o main main.rs && exec ./main
