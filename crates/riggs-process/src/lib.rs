//! Starts each agent in its own process group and kills it with every process it spawned, including
//! the ones that left the group.

mod leader;
mod tail;
mod tree;

pub use leader::{Descendants, Leader, Pipes};
pub use tail::Tail;
