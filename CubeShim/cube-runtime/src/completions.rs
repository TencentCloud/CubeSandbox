// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::{fs, path::PathBuf};

use clap::CommandFactory;
use clap_complete::shells::Bash;

use crate::parser::{CliArgs, CompletionsArgs};
pub fn generate_completions(_args: CompletionsArgs) -> anyhow::Result<()> {
    let mut app = CliArgs::command();
    let user_dir = dirs_next::data_local_dir()
        .unwrap_or_else(|| PathBuf::from("~/.local/share"))
        .join("bash-completion/completions");
    fs::create_dir_all(&user_dir)?;

    let shell = Bash;
    let path = user_dir.join("cube-runtime.bash");
    let mut file = fs::File::create(&path)?;
    clap_complete::generate(shell, &mut app, "cube-runtime", &mut file);

    println!("Generated completions to: {}", path.display());

    Ok(())
}
