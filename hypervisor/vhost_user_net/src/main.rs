// Copyright 2019 Intel Corporation. All Rights Reserved.
//
// Portions Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
//
// SPDX-License-Identifier: (Apache-2.0 AND BSD-3-Clause)

#[macro_use(crate_version, crate_authors)]
extern crate clap;

use clap::{Arg, Command};
use vhost_user_net::start_net_backend;

fn main() {
    env_logger::init();

    let cmd_arguments = Command::new("vhost-user-net backend")
        .version(crate_version!())
        .author(crate_authors!())
        .about("Launch a vhost-user-net backend.")
        .arg(
            Arg::new("net-backend")
                .long("net-backend")
                .help(vhost_user_net::SYNTAX)
                .num_args(1)
                .required(true),
        )
        .get_matches();

    let backend_command = cmd_arguments.get_one::<String>("net-backend").unwrap();
    start_net_backend(backend_command);
}
