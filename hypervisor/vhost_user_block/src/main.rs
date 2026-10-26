// Copyright 2019 Red Hat, Inc. All Rights Reserved.
//
// Portions Copyright 2019 Intel Corporation. All Rights Reserved.
//
// Portions Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Portions Copyright 2017 The Chromium OS Authors. All rights reserved.
//
// SPDX-License-Identifier: (Apache-2.0 AND BSD-3-Clause)

#[macro_use(crate_version, crate_authors)]
extern crate clap;
extern crate vhost_user_block;

use clap::{Arg, Command};
use vhost_user_block::start_block_backend;

fn main() {
    env_logger::init();

    let cmd_arguments = Command::new("vhost-user-blk backend")
        .version(crate_version!())
        .author(crate_authors!())
        .about("Launch a vhost-user-blk backend.")
        .arg(
            Arg::new("block-backend")
                .long("block-backend")
                .help(vhost_user_block::SYNTAX)
                .num_args(1)
                .required(true),
        )
        .get_matches();

    let backend_command = cmd_arguments.get_one::<String>("block-backend").unwrap();
    start_block_backend(backend_command);
}
