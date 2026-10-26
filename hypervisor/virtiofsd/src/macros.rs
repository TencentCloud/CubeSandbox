// Copyright 2022 Red Hat, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

macro_rules! enum_value {
    (
        $(#[$meta:meta])*
        $vis:vis enum $enum:ident: $T:tt {
            $(
                $(#[$variant_meta:meta])*
                $variant:ident $(= $val:expr)?,
            )*
        }
    ) => {
        #[repr($T)]
        $(#[$meta])*
        $vis enum $enum {
            $($(#[$variant_meta])* $variant $(= $val)?,)*
        }

        impl std::convert::TryFrom<$T> for $enum {
            type Error = ();

            fn try_from(v: $T) -> Result<Self, Self::Error> {
                match v {
                    $(v if v == $enum::$variant as $T => Ok($enum::$variant),)*
                    _ => Err(()),
                }
            }
        }
    }
}

pub(crate) use enum_value;
