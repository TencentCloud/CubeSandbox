use std::sync::mpsc::Sender;
use std::sync::Mutex;

use lazy_static::lazy_static;
use log::warn;

lazy_static! {
    static ref NOTIFIER: Mutex<EventNotifier> = Mutex::new(EventNotifier::new());
}

/// Supported Notify Event
///
/// Note VmShutdown event should enable feature `lib_support` for lib.
///
#[derive(Debug, PartialEq, Eq)]
pub enum NotifyEvent {
    VmShutdown,
    VsockServerReady,
    RestoreReady,
    SysStart,
    MigrationComplete,
    MigrationFail,
}

#[derive(Default)]
pub struct EventNotifier {
    notifier: Option<Sender<NotifyEvent>>,
}

impl EventNotifier {
    pub fn new() -> Self {
        Self { notifier: None }
    }
    pub fn init(&mut self, notifier: Sender<NotifyEvent>) {
        self.notifier = Some(notifier);
    }
    pub fn send(&mut self, event: NotifyEvent) {
        if let Some(sender) = self.notifier.as_ref() {
            if let Err(e) = sender.send(event) {
                warn!("event notifier send err: {e}, remove sender.");
                self.notifier.take();
            }
        }
    }
}

pub fn setup_notifier(notifier: Sender<NotifyEvent>) {
    let mut guard = NOTIFIER.lock().unwrap();
    guard.init(notifier);
}

pub fn send_event(event: NotifyEvent) {
    let mut guard = NOTIFIER.lock().unwrap();
    guard.send(event);
}

#[macro_export]
macro_rules! event_notify {
    ($event:expr) => {
        $crate::send_event($event)
    };
}

#[cfg(test)]
mod tests {
    use std::sync::mpsc::channel;
    use std::thread;

    use crate::{setup_notifier, NotifyEvent};

    #[test]
    fn test_event_notify() {
        let (sender, receiver) = channel();
        let handle = thread::spawn(move || {
            setup_notifier(sender);
            event_notify!(NotifyEvent::VmShutdown);
        });
        let evt = receiver.recv().unwrap();
        assert_eq!(evt, NotifyEvent::VmShutdown);
        handle.join().unwrap();
    }
}
