use std::collections::{HashMap, HashSet};

use nix::sys::signal::{Signal, kill as signal, killpg};
use nix::unistd::Pid;
use sysinfo::{ProcessRefreshKind, ProcessesToUpdate, System};

const PASSES: usize = 8;

pub(crate) fn descendants(roots: &[i32]) -> Vec<i32> {
    let children = children_by_parent();
    let mut seen: HashSet<i32> = roots.iter().copied().collect();
    let mut found = Vec::new();
    let mut queue = roots.to_vec();
    while let Some(parent) = queue.pop() {
        for child in children.get(&parent).into_iter().flatten() {
            if seen.insert(*child) {
                found.push(*child);
                queue.push(*child);
            }
        }
    }
    found
}

pub(crate) fn kill(leader: Option<i32>, group: Option<i32>, known: Vec<i32>) {
    let mut roots = known;
    roots.extend(leader);
    for pid in &roots {
        send(*pid, Signal::SIGSTOP);
    }
    let stopped = stop_descendants(&roots);
    if let Some(group) = group.filter(|group| *group > 1) {
        let _ = killpg(Pid::from_raw(group), Signal::SIGKILL);
    }
    for pid in roots.iter().chain(&stopped) {
        send(*pid, Signal::SIGKILL);
    }
}

fn stop_descendants(roots: &[i32]) -> Vec<i32> {
    let mut stopped: HashSet<i32> = roots.iter().copied().collect();
    let mut found = Vec::new();
    for _ in 0..PASSES {
        let fresh = descendants(roots)
            .into_iter()
            .filter(|pid| stopped.insert(*pid))
            .inspect(|pid| {
                send(*pid, Signal::SIGSTOP);
            })
            .collect::<Vec<_>>();
        if fresh.is_empty() {
            break;
        }
        found.extend(fresh);
    }
    found
}

fn send(pid: i32, sig: Signal) -> bool {
    pid > 1 && signal(Pid::from_raw(pid), sig).is_ok()
}

fn children_by_parent() -> HashMap<i32, Vec<i32>> {
    let mut system = System::new();
    system.refresh_processes_specifics(ProcessesToUpdate::All, true, ProcessRefreshKind::nothing());
    let mut children: HashMap<i32, Vec<i32>> = HashMap::new();
    for (pid, process) in system.processes() {
        let parent = process
            .parent()
            .and_then(|parent| i32::try_from(parent.as_u32()).ok());
        if let (Some(parent), Ok(pid)) = (parent, i32::try_from(pid.as_u32())) {
            children.entry(parent).or_default().push(pid);
        }
    }
    children
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]

    use super::*;

    #[test]
    fn our_own_process_is_found_under_its_parent() {
        let me = i32::try_from(std::process::id()).unwrap();
        let parent = i32::try_from(std::os::unix::process::parent_id()).unwrap();
        assert!(descendants(&[parent]).contains(&me));
    }
}
