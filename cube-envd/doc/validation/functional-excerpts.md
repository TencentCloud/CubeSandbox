# Functional validation log excerpts

[English report](../validation.md) · [中文报告](../validation_zh.md)

These are selected records from the completed local runs, not newly executed tests.
JSON records retain only the fields shown; omitted fields include endpoints, tokens,
resource identifiers, image tags, runtime inventories, timestamps and durations.
Text excerpts preserve test names and outcomes; toolchain release dates and
build/test durations are omitted. Build and failure-trace paths are
replaced with `<component-source>`, `<test-source>` and `<temporary-worker>`. Performance records are excluded.

## Rust template and sandbox chain

```jsonl
{"event": "envd_template_created"}
{"event": "envd_template_ready", "status": "READY"}
{"event": "envd_sandbox_created", "provider": "rust"}
{"commit": "2a377149915e550a7f7705824739737de3458898", "event": "envd_identity", "executable": "/usr/bin/envd", "health": 204, "pid": "8", "provider": "rust", "version": "0.5.7"}
{"errors": [], "event": "envd_sandbox_cleanup", "provider": "rust"}
{"errors": [], "event": "envd_template_cleanup"}
{"backend": null, "event": "test_result", "nodeid": "cases/envd/test_public.py::test_new_template_chain", "outcome": "passed", "phase": "call"}
{"backend": null, "event": "test_result", "nodeid": "cases/envd/test_public.py::test_new_template_chain", "outcome": "passed", "phase": "teardown"}
```

## Go template and sandbox chain

```jsonl
{"event": "envd_template_created"}
{"event": "envd_template_ready", "status": "READY"}
{"event": "envd_sandbox_created", "provider": "go"}
{"commit": "b8ca332", "event": "envd_identity", "executable": "/usr/bin/envd", "health": 204, "pid": "8", "provider": "go", "version": "0.5.13"}
{"errors": [], "event": "envd_sandbox_cleanup", "provider": "go"}
{"errors": [], "event": "envd_template_cleanup"}
{"backend": null, "event": "test_result", "nodeid": "cases/envd/test_public.py::test_new_template_chain", "outcome": "passed", "phase": "call"}
{"backend": null, "event": "test_result", "nodeid": "cases/envd/test_public.py::test_new_template_chain", "outcome": "passed", "phase": "teardown"}
```

## Independent platform scenarios

The driver prints each scenario name followed by its process exit code.

```text
go-test_health_identity 0
go-test_sdk_commands 0
go-test_sdk_files 0
rust-test_health_identity 0
rust-test_sdk_commands 0
rust-test_sdk_files 0
```

## Rust component CI and nested Python suites

```text
cargo 1.89.0 (c24e10642)
rustc 1.89.0 (29483883e)
rustfmt 1.8.0-stable (29483883ee)
clippy 0.1.89 (29483883ee)
cargo fmt -- --check
cargo clippy --all-targets --all-features --locked -- -D warnings
    Finished `dev` profile [unoptimized] target(s)
cargo build --locked
    Finished `dev` profile [unoptimized] target(s)
cargo test --all-targets --all-features --locked
    Finished `test` profile [unoptimized] target(s)
test filesystem::snapshot_test::snapshot_server ... ignored, long-running HTTP daemon fixture, explicitly launched inside a snapshot test VM
test cli::tests::accepts_legacy_server_flags ... ok
test cli::tests::accepts_legacy_version_flag ... ok
test error::tests::maps_invalid_argument_to_http_and_connect ... ok
test filesystem::watch::polling::tests::health_rejects_corrupt_watcher_registry_without_a_business_request ... ok
test filesystem::watch::polling::tests::health_rejects_stopped_watcher_reaper_without_draining_events ... ok
test filesystem::watch::polling::tests::observed_accepted_queue_is_not_drained_by_fresh_clients_or_other_watchers ... ok
test filesystem::watch::polling::tests::snapshot_server ... ignored, long-running HTTP observation fixture, explicitly launched in a snapshot test VM
test guest::idle::tests::overlapping_requests_arm_only_after_the_last_response ... ok
test guest::idle::tests::response_completion_error_and_disconnect_release_activity ... ok
test filesystem::watch::polling::tests::captured_get_response_survives_remove_while_new_get_is_not_found ... ok
test guest::idle::tests::timeout_begins_after_a_request_and_ignores_active_requests ... ok
test guest::port_forward::tests::scanner_selects_ipv6_loopback_without_requiring_ipv6_kernel_support ... ok
test guest::port_forward::tests::scanner_selects_only_listening_loopback_tcp ... ok
test process::input::tests::exit_drains_disconnected_close_in_accepted_order ... ok
test process::linux::tests::exec_handshake_requires_complete_setup_and_preserves_errors ... ok
test process::linux::tests::setup_error_classification_distinguishes_exhaustion_and_runtime_failure ... ok
test process::snapshot_test::snapshot_server ... ignored, long-running HTTP daemon fixture, explicitly launched inside a snapshot test VM
test filesystem::watch::polling::tests::removed_root_keeps_its_id_until_explicit_removal ... ok
test init::metadata::tests::metadata_handshake_uses_token_and_bounds_response ... ok
test process::tests::blocking_pty_descriptor_revokes_public_health ... ok
test process::tests::blocking_stderr_descriptor_revokes_public_health ... ok
test process::tests::blocking_stdin_descriptor_revokes_public_health ... ok
test process::tests::cancelled_live_wait_owner_revokes_public_health ... ok
test process::tests::blocking_stdout_descriptor_revokes_public_health ... ok
test process::tests::inheritable_pidfd_revokes_public_health ... ok
test process::tests::concurrent_init_cannot_change_a_preparing_snapshot ... ok
test process::tests::deadline_at_every_preparing_stage_reaps_child_before_returning ... ok
test server::tests::lifecycle_transitions ... ok
test telemetry::tests::redact_does_not_leak_content ... ok
test transport::json::tests::protocol_objects_preserve_wire_types_and_reject_positional_arrays ... ok
test transport::rest::tests::isolated_pure_request_panic_maps_to_internal ... ok
test transport::timeout::tests::timeout_duration_conversion_matches_go_signed_wrapping ... ok
test transport::timeout::tests::timeout_matrix ... ok
test filesystem::watch::polling::tests::public_create_retries_injected_id_collision_without_replacing_existing_watcher ... ok
test process::tests::terminal_reaping_waits_for_atomic_registry_removal ... ok
test process::tests::preparing_failures_are_invisible_and_release_every_reservation ... ok
test result: ok. 34 passed; 0 failed; 3 ignored; 0 measured; 0 filtered out
test result: ok. 0 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test connect_json_unary_error_mapping ... ok
test connect_json_server_stream_emits_start_data_end ... ok
test connect_json_client_stream_roundtrip ... ok
test connect_json_unary_roundtrip ... ok
test connect_request_body_limit_is_resource_exhausted ... ok
test decoded_message_limit_is_resource_exhausted ... ok
test decoded_stream_element_budget_is_resource_exhausted ... ok
test connect_timeout_header_matrix ... ok
test production_connect_accepts_upstream_message_sizes ... ok
test decompressed_message_limit_is_resource_exhausted ... ok
test malformed_and_oversize_stream_frames_return_domain_errors ... ok
test malformed_unary_json_is_bounded_invalid_argument ... ok
test result: ok. 12 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test sigint_and_busy_port_have_distinct_exit_status ... ok
test real_daemon_health_init_auth_cors_and_shutdown ... ok
test cli_identity_and_legacy_parser ... ok
test startup_command_uses_the_managed_process_service ... ok
test non_cgroup_root_falls_back_for_startup_commands_and_pty ... ok
test result: ok. 5 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test fifo_identity_opens_then_reports_upstream_seek_error ... ok
test identity_download_preserves_bytes_and_reports_metadata ... ok
test bound_download_survives_rename_unlink_replacement_and_link_retarget ... ok
test concurrent_truncate_is_a_transport_failure_and_overwrite_is_visible ... ok
test sdk_username_query_selects_the_user_before_path_resolution ... ok
test object_types_signatures_and_shared_path_resolution ... ok
test mime_sniff_replays_prefix_and_disposition_encodes_the_basename ... ok
test stalled_downloads_allow_independent_delivery_and_cancellation ... ok
test gzip_negotiation_uses_oracle_quality_and_wildcard_semantics ... ok
test result: ok. 9 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test download_loop_errors_use_the_file_lookup_status ... ok
test file_method_and_lookup_errors_preserve_status_headers_and_priority ... ok
test identity_seek_errors_follow_preconditions_and_gzip_can_stream ... ok
test multipart_accepts_lf_delimiters_and_headers ... ok
test gzip_framing_buffers_only_small_compressed_responses ... ok
test multipart_continuations_join_utf8_bytes ... ok
test gzip_upload_errors_keep_the_actual_written_prefix ... ok
test multipart_duplicate_error_preserves_first_upload ... ok
test multipart_extended_filename_overrides_plain_name ... ok
test null_device_upload_uses_upstream_device_semantics ... ok
test multipart_decodes_quoted_printable_before_writing ... ok
test null_device_download_matches_upstream ... ok
test single_range_returns_only_the_requested_bytes_and_metadata ... ok
test encoding_fallback_and_permissive_quality_follow_the_file_handler ... ok
test upload_body_validation_precedes_format_and_raw_requires_a_path ... ok
test upload_dispatch_and_multipart_fields_match_the_public_response ... ok
test preconditions_precede_ranges_and_if_range_selects_the_representation ... ok
test range_boundaries_and_multipart_follow_the_upstream_response ... ok
test result: ok. 18 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test gzip_header_metadata_is_bounded_before_mutation ... ok
test bound_upload_survives_retarget_rename_unlink_and_replacement ... ok
test malformed_closing_boundary_is_not_a_successful_upload ... ok
test concurrent_uploads_progress_on_the_same_inode_without_a_path_lock ... ok
test multipart_commits_in_wire_order_and_duplicate_does_not_overwrite ... ok
test multipart_budgets_and_symlink_parent_walk_are_not_lexically_collapsed ... ok
test multipart_override_aliases_nonfiles_and_partial_parser_failure ... ok
test multipart_transport_padding_does_not_swallow_later_files ... ok
test proc_fd_magic_link_writes_the_unlinked_object_not_readlink_text ... ok
test gzip_validation_precedes_mutation_but_bad_trailer_keeps_written_content ... ok
test types_signatures_compose_and_invalid_paths_return_errors ... ok
test octet_upload_preserves_inode_and_creates_parents_and_dangling_target ... ok
test slow_uploads_allow_more_requests_and_disconnect_keeps_only_the_prefix ... ok
test uploads_stream_beyond_previous_file_and_body_limits ... ok
test result: ok. 14 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
......
Ran 6 tests
OK
test signed_files_compose_and_concurrent_transfers ... ok
test result: ok. 1 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test init_does_not_retarget_a_file_request_blocked_in_user_lookup ... ok
test selected_user_home_defaults_and_symlink_dotdot_keep_guest_path_semantics ... ok
test invalid_os_paths_keep_operation_errors_and_root_mutations_are_rejected ... ok
test upload_keeps_captured_default_user_across_concurrent_init ... ok
test result: ok. 4 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test mkdir_missing_prefix_before_dotdot_is_a_successful_creation ... ok
test list_is_depth_first_ordered_and_does_not_recurse_into_child_symlinks ... ok
test mkdir_creates_owned_parents_and_preserves_existing_directory_semantics ... ok
test move_dotdot_destination_reaches_posix_directory_collision ... ok
test remove_absolute_final_dot_preserves_oracle_refusal_before_deleting_children ... ok
test move_creates_destination_parents_and_keeps_posix_collision_errors ... ok
test stat_reports_a_real_file_and_read_default_from_init ... ok
test remove_is_missing_idempotent_and_never_recurses_through_final_symlink ... ok
test stat_symlinks_report_target_type_and_mode_without_a_symlink_enum ... ok
test trailing_slash_retains_stat_and_move_kernel_semantics ... ok
test list_returns_all_entries_beyond_former_count_and_byte_budgets ... ok
test result: ok. 11 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test all_unary_methods_round_trip_raw_protobuf ... ok
test real_eacces_maps_read_and_mutation_failures_without_daemon_failure ... ok
test result: ok. 2 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test default_user_040_threshold_fixture ... ok
test health_is_ready_before_init ... ok
test health_non_ready_is_503 ... ok
test admitted_init_finishes_after_draining_but_new_init_is_rejected ... ok
test init_treats_gzip_bytes_as_malformed_raw_json ... ok
test init_rejects_malformed_non_object_and_excessive_depth ... ok
test init_post_empty_and_body_limit_contract ... ok
test init_rejects_every_non_ready_lifecycle_phase ... ok
test init_validates_known_wire_types_but_ignores_unknown_fields ... ok
test production_server_stays_ready_while_waiting_for_shutdown ... ok
test result: ok. 10 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test concurrent_gets_return_disjoint_ordered_prefixes_without_loss ... ok
test event_burst_is_retained_until_read_and_remove_releases_resources ... ok
test ordered_create_write_remove_over_public_polling ... ok
test initial_recursive_tree_beyond_former_capacity_is_fully_watched ... ok
test rapid_subtree_renames_do_not_reopen_obsolete_names ... ok
test recursive_existing_dynamic_and_populated_move_in_cover_post_observed_mutations ... ok
test polling_survives_create_connection_drains_and_removes ... ok
test root_delete_reports_remove_and_shutdown_cancels ... ok
test root_removal_keeps_events_after_backend_cleanup_and_remove_succeeds ... ok
test root_rename_detaches_original_and_ignores_replacement_and_symlink_retarget ... ok
test subtree_rename_uses_new_path_and_move_out_preserves_older_prefix ... ok
test removed_roots_do_not_evict_older_ids_or_healthy_registry_entries ... ok
test active_watchers_beyond_former_capacity_are_not_evicted_and_remove_reclaims_resources ... ok
test deleted_children_release_recursive_watches ... ok
test result: ok. 14 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test cold_failed_start_does_not_retain_cgroup_or_descriptor ... ok
test result: ok. 1 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test signals_target_only_live_leaders_and_term_is_not_terminal ... ok
test live_input_validation_and_disabled_stdin_close_are_explicit ... ok
test stream_input_fragmentation_order_keepalive_and_eof ... ok
test stdin_full_write_and_ordered_idempotent_close ... ok
test live_deadline_kills_immediately_and_connect_cannot_reset_it ... ok
test term_kill_and_deadline_do_not_signal_background_children ... ok
test timeout_wire_values_preserve_independent_process_cleanup ... ok
test peer_closed_stdin_without_delivery_is_internal ... ok
test result: ok. 8 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test concurrent_payloads_are_serial_and_close_follows_accepted_writes ... ok
test expired_request_deadline_preserves_accepted_write_and_later_input ... ok
test partial_kernel_error_reports_internal_before_terminal ... ok
test queued_inputs_all_complete_and_term_does_not_cancel_writer ... ok
test saturated_writer_does_not_delay_kill_or_deadline_and_subscribers_end ... ok
test result: ok. 5 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test pipe_eof_does_not_end_a_live_leader_and_signal_exit_is_authoritative ... ok
test keepalive_defaults_bounds_and_data_reset ... ok
test immediate_output_and_exit_always_start_first_and_end_once ... ok
test stalled_subscriber_applies_backpressure_and_preserves_complete_output ... ok
test connect_binds_live_identity_without_replay_and_terminal_releases_tag_before_drain ... ok
test result: ok. 5 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test pty_reconnect_input_resize_sigwinch_and_terminal_cleanup ... ok
test pty_start_accepts_default_zero_and_uint16_wrapped_sizes ... ok
test result: ok. 2 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test syscall_signal_errors_keep_registry_and_terminal_wait_authoritative ... ok
test result: ok. 1 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test controls_cannot_target_an_unmanaged_pid ... ok
test absolute_exec_preserves_child_environment_and_setup_precedes_user_code ... ok
test empty_arguments_are_limited_by_exec_not_a_daemon_allocation_budget ... ok
test invalid_start_has_no_pid_and_releases_tag ... ok
test list_starts_empty_and_never_adopts_external_pids ... ok
test protobuf_start_and_list_round_trip_original_config ... ok
test target_credentials_are_applied_before_user_code ... ok
test direct_argv_start_first_and_live_list_preserve_original_config ... ok
test discarding_start_response_keeps_wait_owner_and_terminal_cleanup ... ok
test duplicate_present_tags_and_absent_tags_can_coexist ... ok
test result: ok. 10 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test initial_runtime_state_has_production_defaults_and_immutable_snapshots ... ok
test interrupted_body_before_commit_does_not_mutate_state ... ok
test response_loss_after_complete_request_keeps_the_commit ... ok
test env_and_logical_defaults_commit_atomically_with_effective_generation ... ok
test existing_default_user_and_all_logical_workdir_forms_are_preserved ... ok
test concurrent_timestamp_requests_linearize_to_the_newest_state ... ok
test timestamp_orders_absolute_nanosecond_instants_before_semantic_validation ... ok
test result: ok. 7 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test unexpected_guest_completion_fails_the_server_and_stops_other_tasks ... ok
.s........
Ran 10 tests
OK (skipped=1)
test daemon_guest_startup_services ... ok
test result: ok. 2 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
test invalid_watch_creation_never_starts ... ok
test rapid_subtree_renames_do_not_reopen_obsolete_names ... ok
test recursive_existing_dynamic_and_populated_move_in_cover_post_observed_mutations ... ok
test initial_recursive_tree_beyond_former_capacity_is_fully_watched ... ok
test start_then_ordered_create_write_remove_over_public_stream ... ok
test subtree_rename_uses_new_path_and_move_out_preserves_older_prefix ... ok
test root_rename_detaches_original_and_ignores_replacement_and_symlink_retarget ... ok
test root_delete_reports_remove_and_shutdown_cancels ... ok
test deleted_children_release_recursive_watches ... ok
test result: ok. 9 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out
cargo check --all-targets --all-features --locked
    Checking cube-envd v0.1.0 (<component-source>)
    Finished `dev` profile [unoptimized] target(s)
```

## SDK test framework

```text
122 passed
```

## Rust amd64 image

```text
test_alive_but_unhealthy_daemon_is_visible (test_image.ImageTests) ... ok
test_arguments_environment_log_and_user_exit (test_image.ImageTests) ... ok
test_duplicate_command_and_missing_binary_fail (test_image.ImageTests) ... ok
test_failed_daemon_stops_user_and_descendants (test_image.ImageTests) ... ok
test_installation_and_identity (test_image.ImageTests) ... ok
test_readonly_cgroup_falls_back_and_preserves_supervision (test_image.ImageTests) ... ok
test_startup_failure_is_not_hidden_by_user_command (test_image.ImageTests) ... ok
test_daemon_exit_fails_closed_even_when_clean (test_supervisor.SupervisorTests) ... ok
test_daemon_only_clean_exit_is_failure (test_supervisor.SupervisorTests) ... ok
test_entrypoint_can_be_copied_alone_and_invoked_with_sh (test_supervisor.SupervisorTests) ... ok
test_external_signal_does_not_reach_user_descendant (test_supervisor.SupervisorTests) ... ok
test_external_signals_are_first_cause_and_forwarded_once (test_supervisor.SupervisorTests) ... ok
test_extra_arguments_are_words_without_shell_evaluation (test_supervisor.SupervisorTests) ... ok
test_immediate_user_exit_is_not_lost (test_supervisor.SupervisorTests) ... ok
test_known_second_startup_contract_is_rejected (test_supervisor.SupervisorTests) ... ok
test_missing_executable_and_unwritable_log_fail_before_start (test_supervisor.SupervisorTests) ... ok
test_unresponsive_user_shutdown_is_bounded (test_supervisor.SupervisorTests) ... ok
test_user_status_survives_failed_daemon_shutdown (test_supervisor.SupervisorTests) ... ok

----------------------------------------------------------------------
Ran 18 tests

OK
```

## Go amd64 image

```text
test_alive_but_unhealthy_daemon_is_visible (test_image.ImageTests) ... ok
test_arguments_environment_log_and_user_exit (test_image.ImageTests) ... ok
test_duplicate_command_and_missing_binary_fail (test_image.ImageTests) ... ok
test_failed_daemon_stops_user_and_descendants (test_image.ImageTests) ... ok
test_installation_and_identity (test_image.ImageTests) ... ok
test_readonly_cgroup_falls_back_and_preserves_supervision (test_image.ImageTests) ... ok
test_startup_failure_is_not_hidden_by_user_command (test_image.ImageTests) ... ok
test_daemon_exit_fails_closed_even_when_clean (test_supervisor.SupervisorTests) ... ok
test_daemon_only_clean_exit_is_failure (test_supervisor.SupervisorTests) ... ok
test_entrypoint_can_be_copied_alone_and_invoked_with_sh (test_supervisor.SupervisorTests) ... ok
test_external_signal_does_not_reach_user_descendant (test_supervisor.SupervisorTests) ... ok
test_external_signals_are_first_cause_and_forwarded_once (test_supervisor.SupervisorTests) ... ok
test_extra_arguments_are_words_without_shell_evaluation (test_supervisor.SupervisorTests) ... ok
test_immediate_user_exit_is_not_lost (test_supervisor.SupervisorTests) ... ok
test_known_second_startup_contract_is_rejected (test_supervisor.SupervisorTests) ... ok
test_missing_executable_and_unwritable_log_fail_before_start (test_supervisor.SupervisorTests) ... ok
test_unresponsive_user_shutdown_is_bounded (test_supervisor.SupervisorTests) ... ok
test_user_status_survives_failed_daemon_shutdown (test_supervisor.SupervisorTests) ... ok

----------------------------------------------------------------------
Ran 18 tests

OK
```

## Rust arm64 image on QEMU

```text
test_alive_but_unhealthy_daemon_is_visible (test_image.ImageTests) ... ok
test_arguments_environment_log_and_user_exit (test_image.ImageTests) ... ok
test_duplicate_command_and_missing_binary_fail (test_image.ImageTests) ... ok
test_failed_daemon_stops_user_and_descendants (test_image.ImageTests) ... ok
test_installation_and_identity (test_image.ImageTests) ... ok
test_readonly_cgroup_falls_back_and_preserves_supervision (test_image.ImageTests) ... ok
test_startup_failure_is_not_hidden_by_user_command (test_image.ImageTests) ... ok
test_daemon_exit_fails_closed_even_when_clean (test_supervisor.SupervisorTests) ... ok
test_daemon_only_clean_exit_is_failure (test_supervisor.SupervisorTests) ... ok
test_entrypoint_can_be_copied_alone_and_invoked_with_sh (test_supervisor.SupervisorTests) ... ok
test_external_signal_does_not_reach_user_descendant (test_supervisor.SupervisorTests) ... ok
test_external_signals_are_first_cause_and_forwarded_once (test_supervisor.SupervisorTests) ... ok
test_extra_arguments_are_words_without_shell_evaluation (test_supervisor.SupervisorTests) ... ok
test_immediate_user_exit_is_not_lost (test_supervisor.SupervisorTests) ... ok
test_known_second_startup_contract_is_rejected (test_supervisor.SupervisorTests) ... ok
test_missing_executable_and_unwritable_log_fail_before_start (test_supervisor.SupervisorTests) ... ok
test_unresponsive_user_shutdown_is_bounded (test_supervisor.SupervisorTests) ... ok
test_user_status_survives_failed_daemon_shutdown (test_supervisor.SupervisorTests) ... ok

----------------------------------------------------------------------
Ran 18 tests

OK
```

## Go arm64 image on QEMU — original failure

```text
test_alive_but_unhealthy_daemon_is_visible (test_image.ImageTests) ... ok
test_arguments_environment_log_and_user_exit (test_image.ImageTests) ... ok
test_duplicate_command_and_missing_binary_fail (test_image.ImageTests) ... ok
test_failed_daemon_stops_user_and_descendants (test_image.ImageTests) ... ok
test_installation_and_identity (test_image.ImageTests) ... ok
test_readonly_cgroup_falls_back_and_preserves_supervision (test_image.ImageTests) ... ok
test_startup_failure_is_not_hidden_by_user_command (test_image.ImageTests) ... ok
test_daemon_exit_fails_closed_even_when_clean (test_supervisor.SupervisorTests) ... ok
test_daemon_only_clean_exit_is_failure (test_supervisor.SupervisorTests) ... ok
test_entrypoint_can_be_copied_alone_and_invoked_with_sh (test_supervisor.SupervisorTests) ... ok
test_external_signal_does_not_reach_user_descendant (test_supervisor.SupervisorTests) ... ok
test_external_signals_are_first_cause_and_forwarded_once (test_supervisor.SupervisorTests) ... ok
test_extra_arguments_are_words_without_shell_evaluation (test_supervisor.SupervisorTests) ... ok
test_immediate_user_exit_is_not_lost (test_supervisor.SupervisorTests) ... ok
test_known_second_startup_contract_is_rejected (test_supervisor.SupervisorTests) ... ok
test_missing_executable_and_unwritable_log_fail_before_start (test_supervisor.SupervisorTests) ... ok
test_unresponsive_user_shutdown_is_bounded (test_supervisor.SupervisorTests) ... ok
test_user_status_survives_failed_daemon_shutdown (test_supervisor.SupervisorTests) ... FAIL

======================================================================
FAIL: test_user_status_survives_failed_daemon_shutdown (test_supervisor.SupervisorTests)
----------------------------------------------------------------------
Traceback (most recent call last):
  File "<test-source>", line 91, in test_user_status_survives_failed_daemon_shutdown
    self.finish(23, 'UserExit')
  File "<test-source>", line 84, in finish
    self.assertEqual(self.process.returncode, status, stderr.decode())
AssertionError: 1 != 23 : Traceback (most recent call last):
  File "<temporary-worker>", line 19, in <module>
    sys.exit(int(command.read_text()))
ValueError: invalid literal for int() with base 10: ''
cube-entrypoint: terminal cause=UserExit status=1


----------------------------------------------------------------------
Ran 18 tests

FAILED (failures=1)
```

## Shared supervisor amd64 — after fixture fix

```text
test_daemon_exit_fails_closed_even_when_clean (test_supervisor.SupervisorTests) ... ok
test_daemon_only_clean_exit_is_failure (test_supervisor.SupervisorTests) ... ok
test_entrypoint_can_be_copied_alone_and_invoked_with_sh (test_supervisor.SupervisorTests) ... ok
test_external_signal_does_not_reach_user_descendant (test_supervisor.SupervisorTests) ... ok
test_external_signals_are_first_cause_and_forwarded_once (test_supervisor.SupervisorTests) ... ok
test_extra_arguments_are_words_without_shell_evaluation (test_supervisor.SupervisorTests) ... ok
test_immediate_user_exit_is_not_lost (test_supervisor.SupervisorTests) ... ok
test_known_second_startup_contract_is_rejected (test_supervisor.SupervisorTests) ... ok
test_missing_executable_and_unwritable_log_fail_before_start (test_supervisor.SupervisorTests) ... ok
test_unresponsive_user_shutdown_is_bounded (test_supervisor.SupervisorTests) ... ok
test_user_status_survives_failed_daemon_shutdown (test_supervisor.SupervisorTests) ... ok

----------------------------------------------------------------------
Ran 11 tests

OK
```

## Shared supervisor arm64 on QEMU — after fixture fix

```text
test_daemon_exit_fails_closed_even_when_clean (test_supervisor.SupervisorTests) ... ok
test_daemon_only_clean_exit_is_failure (test_supervisor.SupervisorTests) ... ok
test_entrypoint_can_be_copied_alone_and_invoked_with_sh (test_supervisor.SupervisorTests) ... ok
test_external_signal_does_not_reach_user_descendant (test_supervisor.SupervisorTests) ... ok
test_external_signals_are_first_cause_and_forwarded_once (test_supervisor.SupervisorTests) ... ok
test_extra_arguments_are_words_without_shell_evaluation (test_supervisor.SupervisorTests) ... ok
test_immediate_user_exit_is_not_lost (test_supervisor.SupervisorTests) ... ok
test_known_second_startup_contract_is_rejected (test_supervisor.SupervisorTests) ... ok
test_missing_executable_and_unwritable_log_fail_before_start (test_supervisor.SupervisorTests) ... ok
test_unresponsive_user_shutdown_is_bounded (test_supervisor.SupervisorTests) ... ok
test_user_status_survives_failed_daemon_shutdown (test_supervisor.SupervisorTests) ... ok

----------------------------------------------------------------------
Ran 11 tests

OK
```
