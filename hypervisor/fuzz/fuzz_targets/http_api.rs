// Copyright © 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

#![no_main]
use libfuzzer_sys::fuzz_target;
use micro_http::Request;
use once_cell::sync::Lazy;
use std::os::unix::io::AsRawFd;
use std::sync::mpsc::{channel, Receiver};
use std::thread;
use vmm::api::{http::*, ApiRequest, ApiResponsePayload};
use vmm::{EpollContext, EpollDispatch};
use vmm_sys_util::eventfd::EventFd;

// Need to be ordered for test case reproducibility
static ROUTES: Lazy<Vec<&Box<dyn EndpointHandler + Sync + Send>>> =
    Lazy::new(|| HTTP_ROUTES.routes.values().collect());

fuzz_target!(|bytes| {
    if bytes.len() < 2 {
        return;
    }

    let route = ROUTES[bytes[0] as usize % ROUTES.len()];
    if let Some(request) = generate_request(&bytes[1..]) {
        let exit_evt = EventFd::new(libc::EFD_NONBLOCK).unwrap();
        let api_evt = EventFd::new(libc::EFD_NONBLOCK).unwrap();
        let (api_sender, api_receiver) = channel();

        let http_receiver_thread = {
            let exit_evt = exit_evt.try_clone().unwrap();
            let api_evt = api_evt.try_clone().unwrap();
            thread::Builder::new()
                .name("http_receiver".to_string())
                .spawn(move || {
                    http_receiver_stub(exit_evt, api_evt, api_receiver);
                })
                .unwrap()
        };

        route.handle_request(&request, api_evt, api_sender);
        exit_evt.write(1).ok();
        http_receiver_thread.join().unwrap();
    };
});

fn generate_request(bytes: &[u8]) -> Option<Request> {
    let req_method = match bytes[0] % 5 {
        0 => "GET",
        1 => "PUT",
        2 => "PATCH",
        3 => "POST",
        _ => "INVALID",
    };
    let request_line = format!("{} http://localhost/home HTTP/1.1\r\n", req_method);

    let req_body = &bytes[1..];
    let request = if req_body.len() > 0 {
        [
            format!("{}Content-Length: {}\r\n", request_line, req_body.len()).as_bytes(),
            req_body,
        ]
        .concat()
    } else {
        format!("{}\r\n", request_line).as_bytes().to_vec()
    };

    Request::try_from(&request, None).ok()
}

fn http_receiver_stub(exit_evt: EventFd, api_evt: EventFd, api_receiver: Receiver<ApiRequest>) {
    let mut epoll = EpollContext::new().unwrap();
    epoll.add_event(&exit_evt, EpollDispatch::Exit).unwrap();
    epoll.add_event(&api_evt, EpollDispatch::Api).unwrap();

    let epoll_fd = epoll.as_raw_fd();
    let mut events = vec![epoll::Event::new(epoll::Events::empty(), 0); 2];
    let num_events;
    loop {
        num_events = match epoll::wait(epoll_fd, -1, &mut events[..]) {
            Ok(num_events) => num_events,
            Err(e) => match e.raw_os_error() {
                Some(libc::EAGAIN) | Some(libc::EINTR) => continue,
                _ => panic!("Unexpected epoll::wait error!"),
            },
        };

        break;
    }

    for event in events.iter().take(num_events) {
        let dispatch_event: EpollDispatch = event.data.into();
        match dispatch_event {
            EpollDispatch::Exit => {
                break;
            }
            EpollDispatch::Api => {
                for _ in 0..api_evt.read().unwrap() {
                    let api_request = api_receiver.recv().unwrap();
                    match api_request {
                        ApiRequest::VmCreate(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmDelete(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmBoot(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmShutdown(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmReboot(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmInfo(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmmPing(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmPause(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmResume(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmSnapshot(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmRestore(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmmShutdown(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmResize(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmResizeZone(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmAddDevice(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmAddUserDevice(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmRemoveDevice(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmAddDisk(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmAddFs(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmAddPmem(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmAddNet(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmAddVdpa(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmAddVsock(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmCounters(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmReceiveMigration(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmSendMigration(_, sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmPowerButton(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                        ApiRequest::VmWaitStart(sender) => {
                            sender.send(Ok(ApiResponsePayload::Empty)).unwrap();
                        }
                    }
                }
            }
            _ => {
                panic!("Unexpected Epoll event");
            }
        }
    }
}
