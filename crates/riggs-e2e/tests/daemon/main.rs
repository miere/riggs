//! Runs the real `riggs` binary as a child process against the gateway simulator, so what is
//! asserted is what an operator and a gateway would see.

#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

#[path = "../support/mod.rs"]
mod support;

mod harness;
mod serving;
mod startup;
mod token;
